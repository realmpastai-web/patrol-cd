package applications_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const secretJSON = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","annotations":{"token":"t0p"}},"data":{"password":"aHVudGVyMg=="}}`

func TestGetManifests(t *testing.T) {
	e := newEnv(t, envOptions{cm: map[string]string{"resource.sensitive.mask.annotations": "token"}})
	e.createApp(e.newApp("m", "proj-a"))
	e.repo.mu.Lock()
	e.repo.manifests = map[string][]string{"guestbook": {secretJSON, configMapJSON("cm")}}
	e.repo.mu.Unlock()

	resp, err := e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m", Revision: "v3", NoCache: true})
	require.NoError(t, err)
	require.Len(t, resp.Manifests, 2)
	assert.NotContains(t, resp.Manifests[0], "aHVudGVyMg==")
	assert.NotContains(t, resp.Manifests[0], "t0p")
	assert.Contains(t, resp.Manifests[0], "++++++++")
	assert.Equal(t, configMapJSON("cm"), resp.Manifests[1])
	assert.Equal(t, "rev-v3", resp.Revision)
	assert.Equal(t, inCluster, resp.Server)
	gen := e.repo.lastGenerate()
	assert.Equal(t, "v3", gen.Revision)
	assert.True(t, gen.NoCache)
	assert.Equal(t, "m", gen.AppName)
	assert.Equal(t, "default", gen.Namespace)
	assert.Equal(t, string(v1alpha1.TrackingMethodAnnotation), gen.TrackingMethod)

	_, err = e.client("bob").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m"})
	requireProblem(t, err, 403, "permission denied")
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m", Revisions: []string{"x"}, SourcePositions: []int64{1}})
	requireProblem(t, err, 400, "can only be used with a multi-source application")

	// Repo-server failures: generation errors are 400 with the message, outages 503.
	e.repo.mu.Lock()
	e.repo.genErr = &reposerver.Error{Code: reposerver.CodeInternal, Message: "helm template failed: boom"}
	e.repo.mu.Unlock()
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m"})
	requireProblem(t, err, 400, "error generating manifests: helm template failed: boom")
	e.repo.mu.Lock()
	e.repo.genErr = &reposerver.Error{Code: reposerver.CodeUnavailable, Message: "dial tcp: refused"}
	e.repo.mu.Unlock()
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "m"})
	ae := requireProblem(t, err, 503, "")
	assert.NotContains(t, ae.Detail, "dial tcp")
	e.repo.mu.Lock()
	e.repo.genErr = nil
	e.repo.mu.Unlock()

	// An Application applied with kubectl whose repository the project does not permit is
	// not rendered with the project's credentials.
	rogue := e.newApp("rogue", "proj-a")
	rogue.Spec.Source.RepoURL = "https://user:pw@gitlab.com/private/repo"
	e.createApp(rogue)
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "rogue"})
	ae = requireProblem(t, err, 403, "application repo https://gitlab.com/private/repo is not permitted in project 'proj-a'")
	assert.NotContains(t, ae.Detail, "pw@")

	// Multi-source: positions override revisions; refs are resolved.
	ms := e.newApp("ms", "proj-a")
	ms.Spec.Source = nil
	ms.Spec.Sources = v1alpha1.ApplicationSources{
		{RepoURL: repoURL, Path: "base", TargetRevision: "main", Helm: &v1alpha1.ApplicationSourceHelm{ValueFiles: []string{"$values/v.yaml"}}},
		{RepoURL: "https://github.com/org/values", TargetRevision: "v1", Ref: "values"},
	}
	e.createApp(ms)
	resp, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "ms", Revisions: []string{"v9"}, SourcePositions: []int64{1}})
	require.NoError(t, err)
	assert.Equal(t, []string{"rev-v9", "v1"}, resp.Revisions)
	gen = e.repo.lastGenerate()
	assert.True(t, gen.HasMultipleSources)
	require.Contains(t, gen.RefSources, "$values")
	assert.Equal(t, "v1", gen.RefSources["$values"].TargetRevision)
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "ms", Revisions: []string{"v9"}, SourcePositions: []int64{5}})
	requireProblem(t, err, 400, "source position is out of range")

	// Hydrator Applications render the dry source.
	h := e.newApp("hyd", "proj-a")
	h.Spec.Source = nil
	h.Spec.SourceHydrator = &v1alpha1.SourceHydrator{
		DrySource:  v1alpha1.DrySource{RepoURL: repoURL, TargetRevision: "main", Path: "dry"},
		SyncSource: v1alpha1.SyncSource{TargetBranch: "env/prod", Path: "prod"},
	}
	e.createApp(h)
	_, err = e.client("reader").GetApplicationManifests(e.ctx, &apiclient.ApplicationManifestQuery{Name: "hyd"})
	require.NoError(t, err)
	assert.Equal(t, "dry", e.repo.lastGenerate().Source.Path)
}

func tarGz(t *testing.T, content string) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, err := zw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return b.Bytes()
}

func TestManifestsWithFiles(t *testing.T) {
	e := newEnv(t, envOptions{config: func(_ *testEnv, c *applicationsConfig) { c.MaxLocalFilesBytes = 64 << 10 }})
	e.createApp(e.newApp("loc", "proj-a"))
	e.repo.mu.Lock()
	e.repo.manifests = map[string][]string{"local": {secretJSON}}
	e.repo.mu.Unlock()
	body := tarGz(t, "fake tar")
	sum := sha256.Sum256(body)

	resp, err := e.client("alice").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc", Checksum: hex.EncodeToString(sum[:])}, bytes.NewReader(body))
	require.NoError(t, err)
	require.Len(t, resp.Manifests, 1)
	assert.NotContains(t, resp.Manifests[0], "aHVudGVyMg==")
	e.repo.mu.Lock()
	assert.Equal(t, body, e.repo.filesBodies[len(e.repo.filesBodies)-1])
	e.repo.mu.Unlock()
	assert.Equal(t, "guestbook", e.repo.lastGenerate().Source.Path)
	audits := e.audit.byOperation("getApplicationManifestsWithFiles")
	require.NotEmpty(t, audits)
	assert.Equal(t, "override", audits[len(audits)-1].Action)

	_, err = e.client("reader").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, bytes.NewReader(body))
	requireProblem(t, err, 403, "permission denied: applications, override, proj-a/loc")
	_, err = e.client("bob").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, bytes.NewReader(body))
	requireProblem(t, err, 403, "permission denied")
	_, err = e.client("alice").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc", Checksum: strings.Repeat("0", 64)}, bytes.NewReader(body))
	requireProblem(t, err, 400, "checksum mismatch")
	_, err = e.client("alice").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, strings.NewReader("plain text"))
	requireProblem(t, err, 400, "the request body is not a gzip stream")
	_, err = e.client("alice").GetApplicationManifestsWithFiles(e.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, bytes.NewReader(append(tarGz(t, ""), make([]byte, 70<<10)...)))
	requireProblem(t, err, 413, "")
	// Content type.
	resp2, err := e.client("alice").DoRaw(e.ctx, http.MethodPost, apiclient.PathApplicationManifestsWithFiles,
		&apiclient.ApplicationManifestsWithFilesQuery{Name: "loc"}, bytes.NewReader(body), "text/plain")
	if resp2 != nil {
		_ = resp2.Body.Close()
	}
	requireProblem(t, err, 415, "")
}

// clusterListLimit fails every ClusterList once pass runs out.
type clusterListLimit struct {
	client.Reader
	pass atomic.Int64
}

func (l *clusterListLimit) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*v1alpha1.ClusterList); ok && l.pass.Add(-1) < 0 {
		return errors.New("etcd is not available")
	}
	return l.Reader.List(ctx, list, opts...)
}

// noContextError is what a provider with no credentials for a cluster fails with, as core mode's.
type noContextError struct{}

func (noContextError) Error() string  { return "no kubeconfig context" }
func (noContextError) NoCredentials() {}

// logBuffer is the output of a logger read while the server runs.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// A preview gives what the controller applies, so it renders with the destination's versions: the
// status's, which the controller keeps, or the destination's own answer where the status lacks them.
func TestGetManifestsDestinationVersions(t *testing.T) {
	const edge = "https://edge.example.com"
	reported := v1alpha1.ClusterStatus{ServerVersion: "v9.9.9", Info: v1alpha1.ClusterInfo{APIVersions: []string{"x.example/v1", "x.example/v1/Y"}}}

	// env has an Application at the Cluster edge, whose status is status.
	env := func(t *testing.T, o resEnvOptions, status v1alpha1.ClusterStatus) *resEnv {
		t.Helper()
		re := newResEnv(t, o)
		re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
			p.Spec.Destinations = append(p.Spec.Destinations, v1alpha1.ApplicationDestination{Server: edge, Namespace: "*"})
		})
		cl := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: re.ns}, Spec: v1alpha1.ClusterSpec{Server: edge, Name: "edge"}}
		require.NoError(t, re.kube.Create(re.ctx, cl))
		cl.Status = status
		require.NoError(t, re.kube.Status().Update(re.ctx, cl))
		app := re.newApp("m", "proj-a")
		app.Spec.Destination.Server = edge
		re.createApp(app)
		return re
	}
	get := func(re *resEnv) (*apiclient.ApplicationManifestResponse, error) {
		return re.rclient("reader").GetApplicationManifests(re.ctx, &apiclient.ApplicationManifestQuery{Name: "m"})
	}
	getWithFiles := func(re *resEnv) (*apiclient.ApplicationManifestResponse, error) {
		return re.rclient("alice").GetApplicationManifestsWithFiles(re.ctx, &apiclient.ApplicationManifestsWithFilesQuery{Name: "m"}, bytes.NewReader(tarGz(t, "fake tar")))
	}
	routes := map[string]func(*resEnv) (*apiclient.ApplicationManifestResponse, error){"manifests": get, "manifests with files": getWithFiles}
	versionRequests := func(re *resEnv) []seenRequest {
		return re.cluster.seen(func(r seenRequest) bool { return r.path == "/version" })
	}
	// answered checks that the render had the answer of the fake cluster, which is envtest.
	answered := func(t *testing.T, re *resEnv) {
		t.Helper()
		gen := re.repo.lastGenerate()
		assert.Regexp(t, `^v1\.\d+\.\d+$`, gen.KubeVersion)
		assert.Contains(t, gen.APIVersions, "v1/ConfigMap")
		assert.Contains(t, gen.APIVersions, "apps/v1/Deployment")
		assert.NotContains(t, gen.APIVersions, "x.example/v1/Y")
		assert.Len(t, versionRequests(re), 1)
	}

	for route, render := range routes {
		t.Run("the in-cluster cluster has no object to report on: "+route, func(t *testing.T) {
			re := newResEnv(t, resEnvOptions{})
			re.createApp(re.newApp("m", "proj-a"))
			_, err := render(re)
			require.NoError(t, err)
			answered(t, re)
		})

		t.Run("a status with both is enough: "+route, func(t *testing.T) {
			re := env(t, resEnvOptions{}, reported)
			_, err := render(re)
			require.NoError(t, err)
			gen := re.repo.lastGenerate()
			assert.Equal(t, "v9.9.9", gen.KubeVersion)
			assert.Equal(t, []string{"x.example/v1", "x.example/v1/Y"}, gen.APIVersions)
			assert.Zero(t, re.configs.calls.Load(), "no configuration of the destination")
			assert.Empty(t, versionRequests(re))
		})

		t.Run("an unreported Cluster: "+route, func(t *testing.T) {
			re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
			_, err := render(re)
			require.NoError(t, err)
			answered(t, re)
		})

		t.Run("a destination that does not answer fails the preview: "+route, func(t *testing.T) {
			re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
			gone := httptest.NewServer(http.NotFoundHandler())
			gone.Close()
			re.configs.host = gone.URL
			_, err := render(re)
			p := requireProblem(t, err, 503, "error getting the destination's versions: the destination cluster is not reachable")
			assert.NotContains(t, p.Detail, strings.TrimPrefix(gone.URL, "http://"), "transport details stay in the log")
		})

		for name, status := range map[string]v1alpha1.ClusterStatus{
			"a version alone":    {ServerVersion: "v9.9.9"},
			"API versions alone": {Info: reported.Info},
		} {
			t.Run("a server with no credentials for the destination renders with "+name+": "+route, func(t *testing.T) {
				re := env(t, resEnvOptions{}, status)
				re.configs.err = fmt.Errorf("destination: %w", noContextError{})
				_, err := render(re)
				require.NoError(t, err)
				assert.Equal(t, status.ServerVersion, re.repo.lastGenerate().KubeVersion)
				assert.Equal(t, status.Info.APIVersions, re.repo.lastGenerate().APIVersions)
				assert.EqualValues(t, 1, re.configs.calls.Load())
				assert.Empty(t, versionRequests(re))
			})
		}
	}

	// A preview is a question about the Application, whatever the destination answers.
	for answer, status := range map[string]int{"refuses": http.StatusForbidden, "does not know": http.StatusNotFound, "is busy": http.StatusTooManyRequests} {
		t.Run("a destination that "+answer, func(t *testing.T) {
			re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeStatus(w, status, "the destination's own text")
			}))
			t.Cleanup(srv.Close)
			re.configs.host = srv.URL
			_, err := get(re)
			requireProblem(t, err, 503, "error getting the destination's versions: ")
		})
	}

	t.Run("a destination that redirects is not followed", func(t *testing.T) {
		re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
		var followed atomic.Int64
		other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
		t.Cleanup(other.Close)
		srv := httptest.NewServer(http.RedirectHandler(other.URL+"/version", http.StatusFound))
		t.Cleanup(srv.Close)
		re.configs.host = srv.URL
		_, err := get(re)
		requireProblem(t, err, 503, "error getting the destination's versions: the destination cluster is not reachable")
		assert.Zero(t, followed.Load())
	})

	t.Run("a group version the destination does not describe is left out, and logged", func(t *testing.T) {
		logs := &logBuffer{}
		re := env(t, resEnvOptions{logger: slog.New(slog.NewTextHandler(logs, nil))}, v1alpha1.ClusterStatus{})
		gv := metav1.GroupVersionForDiscovery{GroupVersion: "down.example/v1", Version: "v1"}
		docs := map[string]any{
			"/version": map[string]string{"gitVersion": "v1.30.2"},
			"/api":     metav1.APIVersions{Versions: []string{"v1"}},
			"/apis":    metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "down.example", Versions: []metav1.GroupVersionForDiscovery{gv}, PreferredVersion: gv}}},
			"/api/v1": metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{
				{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: metav1.Verbs{"list", "watch"}},
			}},
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			doc, ok := docs[r.URL.Path]
			if !ok {
				writeStatus(w, http.StatusServiceUnavailable, "the aggregated API is down")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
		}))
		t.Cleanup(srv.Close)
		re.configs.host = srv.URL
		_, err := get(re)
		require.NoError(t, err)
		gen := re.repo.lastGenerate()
		assert.Equal(t, "v1.30.2", gen.KubeVersion)
		assert.Equal(t, []string{"v1", "v1/ConfigMap"}, gen.APIVersions)
		assert.Contains(t, logs.String(), "group=down.example")
		assert.Contains(t, logs.String(), "cluster=edge")
	})

	for name, status := range map[string]v1alpha1.ClusterStatus{
		"a version alone":    {ServerVersion: "v9.9.9"},
		"API versions alone": {Info: reported.Info},
	} {
		t.Run(name+" does not describe the destination", func(t *testing.T) {
			re := env(t, resEnvOptions{}, status)
			_, err := get(re)
			require.NoError(t, err)
			answered(t, re)
		})
	}

	t.Run("a destination named, which the project permits by server", func(t *testing.T) {
		re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
		app := re.getApp(re.ns, "m")
		app.Spec.Destination = v1alpha1.ApplicationDestination{Name: "edge", Namespace: "default"}
		require.NoError(t, re.kube.Update(re.ctx, app))
		_, err := get(re)
		require.NoError(t, err)
		answered(t, re)
	})

	// An Application applied with kubectl is not validated, and a reader's preview must not be a way
	// to reach a cluster its project does not permit.
	for name, status := range map[string]v1alpha1.ClusterStatus{
		"a version alone":    {ServerVersion: "v9.9.9"},
		"API versions alone": {Info: reported.Info},
	} {
		t.Run("a destination the project does not permit is not contacted, with "+name, func(t *testing.T) {
			re := env(t, resEnvOptions{}, status)
			re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
				p.Spec.Destinations = []v1alpha1.ApplicationDestination{{Server: inCluster, Namespace: "*"}}
			})
			_, err := get(re)
			require.NoError(t, err)
			assert.Equal(t, status.ServerVersion, re.repo.lastGenerate().KubeVersion)
			assert.Equal(t, status.Info.APIVersions, re.repo.lastGenerate().APIVersions)
			assert.Zero(t, re.configs.calls.Load())
			assert.Empty(t, versionRequests(re))
		})
	}

	t.Run("a namespace the project does not permit is not contacted", func(t *testing.T) {
		re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
		re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
			p.Spec.Destinations = []v1alpha1.ApplicationDestination{{Server: edge, Namespace: "elsewhere"}}
		})
		_, err := get(re)
		require.NoError(t, err)
		assert.Zero(t, re.configs.calls.Load())
		assert.Empty(t, versionRequests(re))
	})

	t.Run("the namespace the project permits", func(t *testing.T) {
		re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
		re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
			p.Spec.Destinations = []v1alpha1.ApplicationDestination{{Server: edge, Namespace: "default"}}
		})
		_, err := get(re)
		require.NoError(t, err)
		answered(t, re)
	})

	t.Run("a project that permits only its own clusters", func(t *testing.T) {
		re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
		re.updateProject("proj-a", func(p *v1alpha1.AppProject) { p.Spec.PermitOnlyProjectScopedClusters = true })
		_, err := get(re)
		require.NoError(t, err)
		assert.Empty(t, re.repo.lastGenerate().KubeVersion)
		assert.Zero(t, re.configs.calls.Load(), "the Cluster is not the project's")

		cl := &v1alpha1.Cluster{}
		require.NoError(t, re.kube.Get(re.ctx, client.ObjectKey{Namespace: re.ns, Name: "edge"}, cl))
		cl.Spec.Project = "proj-a"
		require.NoError(t, re.kube.Update(re.ctx, cl))
		_, err = get(re)
		require.NoError(t, err)
		answered(t, re)
	})

	t.Run("a permission that cannot be checked", func(t *testing.T) {
		limit := &clusterListLimit{}
		limit.pass.Store(math.MaxInt64)
		re := env(t, resEnvOptions{reader: func(r client.Reader) client.Reader {
			limit.Reader = r
			return limit
		}}, v1alpha1.ClusterStatus{ServerVersion: "v9.9.9"})
		re.updateProject("proj-a", func(p *v1alpha1.AppProject) { p.Spec.PermitOnlyProjectScopedClusters = true })
		limit.pass.Store(2) // the two reads before it: the Application's project and its destination
		_, err := get(re)
		p := requireProblem(t, err, 503, "error checking the destination cluster")
		assert.NotContains(t, p.Detail, "etcd")
		assert.Zero(t, re.configs.calls.Load())
	})

	t.Run("a configuration that cannot be built", func(t *testing.T) {
		re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
		re.configs.err = errors.New("read secret edge-token: forbidden")
		_, err := get(re)
		p := requireProblem(t, err, 503, "error getting application cluster config: the configuration of")
		assert.NotContains(t, p.Detail, "edge-token")
	})

	t.Run("one discovery for the sources of a preview", func(t *testing.T) {
		re := env(t, resEnvOptions{}, v1alpha1.ClusterStatus{})
		app := re.getApp(re.ns, "m")
		app.Spec.Source = nil
		app.Spec.Sources = v1alpha1.ApplicationSources{
			{RepoURL: repoURL, Path: "base", TargetRevision: "main"},
			{RepoURL: repoURL, Path: "overlay", TargetRevision: "main"},
		}
		require.NoError(t, re.kube.Update(re.ctx, app))
		_, err := get(re)
		require.NoError(t, err)
		answered(t, re)
	})

	// The controller's cache of the cluster reads these with the cluster's credentials: a service
	// account of the project might not be allowed to, and would change the answer.
	t.Run("impersonation does not apply", func(t *testing.T) {
		re := env(t, resEnvOptions{cm: map[string]string{"application.sync.impersonation.enabled": "true"}}, v1alpha1.ClusterStatus{})
		_, err := get(re)
		require.NoError(t, err, "with no matching service account, an impersonated call is refused")
		answered(t, re)

		re = env(t, resEnvOptions{cm: map[string]string{"application.sync.impersonation.enabled": "true"}}, v1alpha1.ClusterStatus{})
		re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
			p.Spec.DestinationServiceAccounts = []v1alpha1.ApplicationDestinationServiceAccount{{Server: edge, Namespace: "*", DefaultServiceAccount: "default:deployer"}}
		})
		_, err = get(re)
		require.NoError(t, err)
		answered(t, re)
		for _, r := range re.cluster.seen(func(seenRequest) bool { return true }) {
			assert.Empty(t, r.impersonate, r.path)
		}
	})
}

// Validating an Application reads only the status, so that a create does not depend on its
// destination, and a hydrator's dry source is checked without cluster data.
func TestValidationRendersWithTheStatus(t *testing.T) {
	const edge = "https://edge.example.com"
	re := newResEnv(t, resEnvOptions{})
	re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.Destinations = append(p.Spec.Destinations, v1alpha1.ApplicationDestination{Server: edge, Namespace: "*"})
	})
	cl := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: re.ns}, Spec: v1alpha1.ClusterSpec{Server: edge, Name: "edge"}}
	require.NoError(t, re.kube.Create(re.ctx, cl))
	cl.Status = v1alpha1.ClusterStatus{ServerVersion: "v9.9.9", Info: v1alpha1.ClusterInfo{APIVersions: []string{"x.example/v1", "x.example/v1/Y"}}}
	require.NoError(t, re.kube.Status().Update(re.ctx, cl))

	plain := re.newApp("plain", "proj-a")
	plain.Spec.Destination.Server = edge
	_, err := re.rclient("alice").CreateApplication(re.ctx, &apiclient.ApplicationCreateRequest{Application: plain})
	require.NoError(t, err)
	gen := re.repo.lastGenerate()
	assert.Equal(t, "v9.9.9", gen.KubeVersion)
	assert.Equal(t, cl.Status.Info.APIVersions, gen.APIVersions)

	hydrated := re.newApp("hydrated", "proj-a")
	hydrated.Spec.Destination.Server = edge
	hydrated.Spec.Source = nil
	hydrated.Spec.SourceHydrator = &v1alpha1.SourceHydrator{
		DrySource:  v1alpha1.DrySource{RepoURL: repoURL, TargetRevision: "main", Path: "dry"},
		SyncSource: v1alpha1.SyncSource{TargetBranch: "env/prod", Path: "prod"},
	}
	_, err = re.rclient("alice").CreateApplication(re.ctx, &apiclient.ApplicationCreateRequest{Application: hydrated})
	require.NoError(t, err)
	gen = re.repo.lastGenerate()
	assert.Equal(t, "dry", gen.Source.Path)
	assert.Empty(t, gen.KubeVersion)
	assert.Empty(t, gen.APIVersions)

	// A preview would ask the destination here.
	const unreported = "https://unreported.example.com"
	re.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.Destinations = append(p.Spec.Destinations, v1alpha1.ApplicationDestination{Server: unreported, Namespace: "*"})
	})
	partial := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "unreported", Namespace: re.ns}, Spec: v1alpha1.ClusterSpec{Server: unreported, Name: "unreported"}}
	require.NoError(t, re.kube.Create(re.ctx, partial))
	partial.Status = v1alpha1.ClusterStatus{ServerVersion: "v9.9.8"}
	require.NoError(t, re.kube.Status().Update(re.ctx, partial))
	app := re.newApp("unreported", "proj-a")
	app.Spec.Destination.Server = unreported
	_, err = re.rclient("alice").CreateApplication(re.ctx, &apiclient.ApplicationCreateRequest{Application: app})
	require.NoError(t, err)
	gen = re.repo.lastGenerate()
	assert.Equal(t, "v9.9.8", gen.KubeVersion)
	assert.Empty(t, gen.APIVersions)

	assert.Zero(t, re.configs.calls.Load(), "no configuration of the destination")
}

func TestRevisionMetadata(t *testing.T) {
	e := newEnv(t, envOptions{})
	app := e.createApp(e.newApp("rev", "proj-a"))
	app.Status.History = v1alpha1.RevisionHistories{
		{ID: 1, Revision: "old", DeployedAt: metav1.Now(), Source: &v1alpha1.ApplicationSource{RepoURL: "https://github.com/org/old", Path: "p"}},
	}
	require.NoError(t, e.kube.Status().Update(e.ctx, app))
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	e.repo.mu.Lock()
	e.repo.metadata = &reposerver.RevisionMetadata{Author: "Jane <jane@example.com>", Date: when, Message: "fix", Tags: []string{"v1"},
		References: []reposerver.RevisionReference{{RepoURL: "https://u:p@github.com/org/dry", SHA: "abc"}}}
	e.repo.chart = &reposerver.ChartDetails{Description: "chart", Home: "https://example.com"}
	e.repo.oci = &reposerver.OCIMetadata{Version: "1.2.3"}
	e.repo.mu.Unlock()
	reader := e.client("reader")

	md, err := reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: sha})
	require.NoError(t, err)
	assert.Equal(t, "Jane <jane@example.com>", md.Author)
	require.NotNil(t, md.Date)
	assert.True(t, when.Equal(md.Date.Time))
	require.Len(t, md.References, 1)
	assert.Equal(t, "https://github.com/org/dry", md.References[0].RepoURL)
	e.repo.mu.Lock()
	last := e.repo.metaReqs[len(e.repo.metaReqs)-1]
	e.repo.mu.Unlock()
	assert.Equal(t, sha, last.Revision)
	assert.Equal(t, repoURL, last.Repo.URL)

	// versionId selects the history entry's source.
	vid := int32(1)
	_, err = reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: "old", VersionID: &vid})
	require.NoError(t, err)
	e.repo.mu.Lock()
	last = e.repo.metaReqs[len(e.repo.metaReqs)-1]
	e.repo.mu.Unlock()
	assert.Equal(t, "https://github.com/org/old", last.Repo.URL)

	idx := int32(1)
	_, err = reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: sha, SourceIndex: &idx})
	requireProblem(t, err, 400, "source index 1 not found because there is only 1 source")
	missing := int32(9)
	_, err = reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: sha, VersionID: &missing})
	requireProblem(t, err, 400, "revision history not found for version ID 9")
	_, err = e.client("bob").GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: sha})
	requireProblem(t, err, 403, "permission denied")

	_, err = reader.GetApplicationRevisionChartDetails(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: "1.0.0"})
	requireProblem(t, err, 400, "no chart found for application: rev")
	chartApp := e.newApp("chart", "proj-a")
	chartApp.Spec.Source = &v1alpha1.ApplicationSource{RepoURL: "https://github.com/org/charts", Chart: "nginx", TargetRevision: "1.0.0"}
	e.createApp(chartApp)
	cd, err := reader.GetApplicationRevisionChartDetails(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "chart", Revision: "1.0.0"})
	require.NoError(t, err)
	assert.Equal(t, "chart", cd.Description)
	oci, err := reader.GetApplicationRevisionOCIMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "chart", Revision: "1.0.0"})
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", oci.Version)
	e.repo.mu.Lock()
	assert.Equal(t, "nginx", e.repo.chartReqs[len(e.repo.chartReqs)-1].Name)
	e.repo.mu.Unlock()

	e.repo.mu.Lock()
	e.repo.metadata = nil
	e.repo.mu.Unlock()
	_, err = reader.GetApplicationRevisionMetadata(e.ctx, &apiclient.ApplicationRevisionRequest{Name: "rev", Revision: "nope"})
	requireProblem(t, err, 400, "error getting revision metadata: revision not found")
}

func TestApplicationLinks(t *testing.T) {
	e := newEnv(t, envOptions{cm: map[string]string{
		"application.links": `- url: https://grafana.example.com/d/{{ .app.metadata.name }}?ns={{ .app.spec.destination.namespace }}
  title: Grafana
- url: https://only-prod.example.com
  title: Prod only
  if: app.metadata.labels.env == "prod"
- url: '{{ .managedByURL }}/applications/{{ .app.metadata.name }}'
  title: Self
- url: 'javascript:alert(1)'
  title: Script
- url: https://bad.example.com
  title: Bad condition
  if: '"not a bool"'`,
		"resource.links": `- url: https://logs.example.com/{{ .resource.metadata.name }}/{{ .resource.data.password }}
  title: Logs
  if: resource.kind == "Secret"
- url: https://apps.example.com/{{ .resource.metadata.name }}/{{ .project.metadata.name }}
  title: App
  if: resource.kind == "Application"`,
	}})
	app := e.newApp("lnk", "proj-a")
	app.Labels = map[string]string{"env": "dev"}
	e.createApp(app)
	reader := e.client("reader")

	l, err := reader.ListApplicationLinks(e.ctx, &apiclient.ApplicationLinksRequest{Name: "lnk"})
	require.NoError(t, err)
	var titles []string
	for _, i := range l.Items {
		titles = append(titles, i.Title)
	}
	assert.Equal(t, []string{"Grafana", "Self"}, titles)
	assert.Equal(t, "https://grafana.example.com/d/lnk?ns=default", l.Items[0].URL)
	assert.Equal(t, "https://patrol.example.com/applications/lnk", l.Items[1].URL)

	e.updateApp("lnk", func(a *v1alpha1.Application) {
		a.Labels["env"] = "prod"
		a.Annotations = map[string]string{v1alpha1.AnnotationKeyManagedByURL: "https://other.example.com"}
	})
	l, err = reader.ListApplicationLinks(e.ctx, &apiclient.ApplicationLinksRequest{Name: "lnk"})
	require.NoError(t, err)
	require.Len(t, l.Items, 3)
	assert.Equal(t, "https://other.example.com/applications/lnk", l.Items[2].URL)

	_, err = e.client("bob").ListApplicationLinks(e.ctx, &apiclient.ApplicationLinksRequest{Name: "lnk"})
	requireProblem(t, err, 403, "permission denied")

	// Resource links: the Application itself.
	rl, err := reader.ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "lnk", Group: v1alpha1.GroupName, Kind: "Application", ResourceName: "lnk"})
	require.NoError(t, err)
	require.Len(t, rl.Items, 1)
	assert.Equal(t, "https://apps.example.com/lnk/proj-a", rl.Items[0].URL)

	// A managed Secret: its data is masked before templates see it.
	e.live.objs["/Secret/default/db"] = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "db", "namespace": "default"},
		"data": map[string]any{"password": "aHVudGVyMg=="},
	}}
	rl, err = reader.ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "lnk", Kind: "Secret", Namespace: "default", ResourceName: "db", Version: "v1"})
	require.NoError(t, err)
	require.Len(t, rl.Items, 1)
	assert.Equal(t, "https://logs.example.com/db/++++++++", rl.Items[0].URL)

	_, err = reader.ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "lnk", Kind: "Pod", Namespace: "default", ResourceName: "nope"})
	requireProblem(t, err, 400, "Pod  nope not found as part of application lnk")
	_, err = reader.ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "lnk"})
	requireProblem(t, err, 400, "kind and resourceName are required")

	// The project must permit the destination.
	e.updateProject("proj-a", func(p *v1alpha1.AppProject) {
		p.Spec.Destinations = []v1alpha1.ApplicationDestination{{Server: inCluster, Namespace: "elsewhere"}}
	})
	_, err = reader.ListApplicationLinks(e.ctx, &apiclient.ApplicationLinksRequest{Name: "lnk"})
	requireProblem(t, err, 400, "error getting destination cluster")
}

func TestResourceLinksWithoutLiveAccess(t *testing.T) {
	e := newEnv(t, envOptions{noLive: true})
	e.createApp(e.newApp("x", "proj-a"))
	_, err := e.client("reader").ListApplicationResourceLinks(e.ctx, &apiclient.ApplicationResourceRequest{Name: "x", Kind: "Pod", ResourceName: "p"})
	requireProblem(t, err, 503, "live resource access is not available")
}
