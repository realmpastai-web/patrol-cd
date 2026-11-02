package integration

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/admin"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/server"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
	"github.com/juliosaraiva/patrol-cd/test/integration/stack"
)

// fastClusterInfo shortens the time a test waits for the cluster-info updater.
func fastClusterInfo(c *server.ControllerConfig) { c.ClusterInfoInterval = time.Second }

// waitCluster waits until the API reports a cluster that cond accepts.
func waitCluster(t *testing.T, s *stack.Stack, d *stack.Cluster, what string, cond func(*apiclient.RegisteredCluster) bool) *apiclient.RegisteredCluster {
	t.Helper()
	var last *apiclient.RegisteredCluster
	stack.Eventually(t, time.Minute, what, func() (bool, string) {
		c, err := s.API(t).GetCluster(t.Context(), apiclient.ClusterByServer(d.Server))
		if err != nil {
			return false, err.Error()
		}
		last = c
		return cond(c), fmt.Sprintf("labels=%v status=%+v connection=%+v", c.Metadata.Labels, c.Status, c.Status.ConnectionState)
	})
	return last
}

func connectionIs(c *apiclient.RegisteredCluster, status v1alpha1.ConnectionStatus) bool {
	return c.Status.ConnectionState != nil && c.Status.ConnectionState.Status == status
}

// TestClusterInfoWrittenByController: RunController keeps the status of the clusters of its shards,
// as the API and the CLI show it.
func TestClusterInfoWrittenByController(t *testing.T) {
	s := stack.Start(t, stack.Options{DedicatedDestinations: 2, Controller: fastClusterInfo})
	used, idle := s.Destination(2), s.Destination(3)
	ns := s.NewNamespace(t, used, "clusterinfo")
	repo := s.Git().NewRepo(t, "clusterinfo")
	repo.CommitFiles(t, "v1", map[string]string{"app/cm.yaml": configMap("cm", map[string]string{"v": "1"}, nil)})
	s.CreateApp(t, s.NewApp("by-server", repo, "app", used, ns))
	byName := s.NewApp("by-name", repo, "app", used, ns)
	byName.Spec.Destination.Server, byName.Spec.Destination.Name = "", used.Name
	s.CreateApp(t, byName)

	c := waitCluster(t, s, used, "the status of a cluster with Applications", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusSuccessful) && c.Status.Info.ApplicationsCount == 2
	})
	assert.NotEmpty(t, c.Status.ServerVersion)
	assert.Contains(t, c.Status.Info.APIVersions, "v1/ConfigMap")
	assert.Contains(t, c.Status.Info.APIVersions, "apps/v1/Deployment")
	assert.Positive(t, c.Status.Info.CacheInfo.APIsCount)
	assert.Positive(t, c.Status.Info.CacheInfo.ResourcesCount)
	assert.NotNil(t, c.Status.Info.CacheInfo.LastCacheSyncTime)
	assert.Empty(t, c.Metadata.Labels[v1alpha1.LabelKeyClusterKubernetesVersion], "the version label is opt-in")

	unused := waitCluster(t, s, idle, "the status of a cluster without Applications", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusUnknown)
	})
	assert.Equal(t, clusters.NoApplicationsMessage, unused.Status.ConnectionState.Message)
	assert.Empty(t, unused.Status.ServerVersion)

	_, err := s.API(t).UpdateCluster(t.Context(), apiclient.ClusterByServer(used.Server), &apiclient.ClusterInput{
		Server: used.Server,
		Labels: map[string]string{v1alpha1.LabelKeyAutoLabelClusterInfo: "true"},
	}, "labels")
	require.NoError(t, err)
	waitCluster(t, s, used, "the version label of a cluster that opted in", func(l *apiclient.RegisteredCluster) bool {
		return l.Metadata.Labels[v1alpha1.LabelKeyClusterKubernetesVersion] == c.Status.ServerVersion
	})

	_, err = s.API(t).UpdateCluster(t.Context(), apiclient.ClusterByServer(used.Server), &apiclient.ClusterInput{
		Server: used.Server,
		Labels: map[string]string{v1alpha1.LabelKeyAutoLabelClusterInfo: "true", v1alpha1.LabelKeyClusterKubernetesVersion: "v0.0.0"},
	}, "labels")
	require.NoError(t, err)
	waitCluster(t, s, used, "a stale version label replaced", func(l *apiclient.RegisteredCluster) bool {
		return l.Metadata.Labels[v1alpha1.LabelKeyClusterKubernetesVersion] == c.Status.ServerVersion
	})

	row := func(d *stack.Cluster) string {
		for line := range strings.Lines(s.CLI(t, "cluster", "list").Stdout) {
			if strings.HasPrefix(line, d.Server+" ") {
				return line
			}
		}
		return ""
	}
	assert.Contains(t, row(used), c.Status.ServerVersion)
	assert.Contains(t, row(used), string(v1alpha1.ConnectionStatusSuccessful))
	assert.Contains(t, row(idle), string(v1alpha1.ConnectionStatusUnknown))

	// patrol admin reads the same status directly from the control cluster.
	var stats strings.Builder
	admincmd := admin.NewCommand(&admin.Factory{
		IOStreams:  admin.IOStreams{Out: &stats, ErrOut: io.Discard},
		RESTConfig: func() (*rest.Config, string, error) { return s.Control().Config, s.Namespace, nil },
	})
	admincmd.SetArgs([]string{"cluster", "stats", "--replicas", "1"})
	require.NoError(t, admincmd.ExecuteContext(t.Context()))
	var fields []string
	for line := range strings.Lines(stats.String()) {
		if strings.HasPrefix(line, used.Server+" ") {
			fields = strings.Fields(line)
		}
	}
	require.Len(t, fields, 6, "SERVER SHARD CONNECTION NAMESPACES APPS RESOURCES:\n%s", stats.String())
	assert.Equal(t, string(v1alpha1.ConnectionStatusSuccessful), fields[2])
	assert.Equal(t, "2", fields[4])
	resources, err := strconv.Atoi(fields[5])
	require.NoError(t, err)
	assert.Positive(t, resources)
}

// gatedChart defines an API of group on d and commits a chart whose ConfigMap "gated" renders only for
// a destination that serves it. It returns the API, as a chart names it, with the repository and the
// revision.
func gatedChart(t *testing.T, s *stack.Stack, d *stack.Cluster, group string) (string, *stack.Repo, string) {
	t.Helper()
	d.Apply(t, "", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.`+group+`
spec:
  group: `+group+`
  scope: Namespaced
  names: {kind: Widget, plural: widgets, singular: widget}
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          x-kubernetes-preserve-unknown-fields: true
`)
	widgets := group + "/v1/Widget"
	repo := s.Git().NewRepo(t, "capabilities")
	rev := repo.CommitFiles(t, "v1", map[string]string{
		"chart/Chart.yaml": "apiVersion: v2\nname: capabilities\nversion: 0.1.0\n",
		"chart/templates/gated.yaml": `{{- if .Capabilities.APIVersions.Has "` + widgets + `" }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: gated
{{- end }}
`,
		"chart/templates/always.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: always\n",
	})
	return widgets, repo, rev
}

// TestManifestsMatchControllerCapabilities: GET .../manifests renders with the API versions of the
// Cluster status, so a chart gated on an API of the destination gives what the controller applied.
func TestManifestsMatchControllerCapabilities(t *testing.T) {
	s := stack.Start(t, stack.Options{DedicatedDestinations: 1, Controller: fastClusterInfo})
	d := s.Destination(2)
	ns := s.NewNamespace(t, d, "capabilities")
	widgets, repo, rev := gatedChart(t, s, d, "patrol-test.example.com")
	s.CreateApp(t, s.NewApp("capabilities", repo, "chart", d, ns))
	requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "capabilities"}))
	s.WaitSyncedHealthy(t, "capabilities", rev)
	require.True(t, d.Exists(t, "v1", "ConfigMap", ns, "gated"), "the controller rendered with the destination's APIs")

	waitCluster(t, s, d, "the status lists the destination's API", func(c *apiclient.RegisteredCluster) bool {
		return slices.Contains(c.Status.Info.APIVersions, widgets)
	})
	// GET manifests reads the Cluster through the API server's informer, which can trail that read.
	stack.Eventually(t, time.Minute, "manifests rendered with the destination's API", func() (bool, string) {
		got, err := s.API(t).GetApplicationManifests(t.Context(), &apiclient.ApplicationManifestQuery{Name: "capabilities"})
		if err != nil {
			return false, err.Error()
		}
		joined := strings.Join(got.Manifests, "\n")
		return strings.Contains(joined, `"gated"`), joined
	})
}

// TestManifestsOfTheInClusterCluster: the in-cluster cluster has no Cluster object to hold a status, so
// GET .../manifests asks the cluster itself and a gated chart still gives what the controller applied.
func TestManifestsOfTheInClusterCluster(t *testing.T) {
	s := stack.Start(t, stack.Options{})
	d := s.Destination(0)
	ns := s.NewNamespace(t, d, "capabilities")
	group := "patrol-test-" + s.ID + ".example.com"
	_, repo, rev := gatedChart(t, s, d, group)
	defer d.Delete(t, "apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "widgets."+group)
	s.CreateApp(t, s.NewApp("capabilities", repo, "chart", d, ns))
	requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "capabilities"}))
	s.WaitSyncedHealthy(t, "capabilities", rev)
	require.True(t, d.Exists(t, "v1", "ConfigMap", ns, "gated"), "the controller rendered with the destination's APIs")

	got, err := s.API(t).GetApplicationManifests(t.Context(), &apiclient.ApplicationManifestQuery{Name: "capabilities"})
	require.NoError(t, err)
	assert.Contains(t, strings.Join(got.Manifests, "\n"), `"gated"`)
}

// TestRenderInputsOutliveTheCache: validation and GET .../manifests read the version and API versions
// of the Cluster status, so these stay when the controller has nothing newer to say. A cluster with
// no Applications keeps what the API's probe found, and a skip-reconcile cluster what its cache
// reported. A controller that restarted has no cache either, and goes the same way.
func TestRenderInputsOutliveTheCache(t *testing.T) {
	s := stack.Start(t, stack.Options{DedicatedDestinations: 1, UnregisteredDedicated: true, Controller: fastClusterInfo})
	d := s.Destination(2)
	creds := d.Credentials()
	_, err := s.API(t).CreateCluster(t.Context(), &apiclient.ClusterInput{
		Server: d.Server, Name: "render-inputs",
		Config: apiclient.ClusterConfigInput{
			BearerToken:     string(creds[v1alpha1.ClusterSecretKeyBearerToken]),
			TLSClientConfig: apiclient.ClusterTLSClientConfigInput{CAData: creds[v1alpha1.ClusterSecretKeyCAData]},
		},
	}, false)
	require.NoError(t, err)
	unused := waitCluster(t, s, d, "the probed version of a cluster with no Applications", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusUnknown) && c.Status.ConnectionState.Message == clusters.NoApplicationsMessage && c.Status.ServerVersion != ""
	})
	version := unused.Status.ServerVersion

	ns := s.NewNamespace(t, d, "render-inputs")
	repo := s.Git().NewRepo(t, "render-inputs")
	rev := repo.CommitFiles(t, "v1", map[string]string{
		// Only that version: the create below is refused if validation renders with any other.
		"chart/Chart.yaml":        "apiVersion: v2\nname: render-inputs\nversion: 0.1.0\nkubeVersion: \"=" + strings.TrimPrefix(version, "v") + "\"\n",
		"chart/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kube\ndata:\n  version: {{ .Capabilities.KubeVersion.Version | quote }}\n",
	})
	s.CreateApp(t, s.NewApp("render-inputs", repo, "chart", d, ns))
	requireSucceeded(t, syncAndWait(t, s, &apiclient.ApplicationSyncRequest{Name: "render-inputs"}))
	s.WaitSyncedHealthy(t, "render-inputs", rev)
	used := waitCluster(t, s, d, "the status of the cluster in use", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusSuccessful) && len(c.Status.Info.APIVersions) > 0
	})

	manifests := func() string {
		t.Helper()
		got, err := s.API(t).GetApplicationManifests(t.Context(), &apiclient.ApplicationManifestQuery{Name: "render-inputs"})
		require.NoError(t, err)
		return strings.Join(got.Manifests, "\n")
	}
	rendered := `"version":"` + version + `"`
	assert.Contains(t, manifests(), rendered)

	_, err = s.API(t).UpdateCluster(t.Context(), apiclient.ClusterByServer(d.Server), &apiclient.ClusterInput{
		Server:      d.Server,
		Annotations: map[string]string{v1alpha1.AnnotationKeySkipReconcile: "true"},
	}, "annotations")
	require.NoError(t, err)
	skipped := waitCluster(t, s, d, "a skip-reconcile cluster reports no state", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusUnknown)
	})
	assert.Equal(t, version, skipped.Status.ServerVersion)
	assert.Equal(t, used.Status.Info.APIVersions, skipped.Status.Info.APIVersions)
	assert.Contains(t, manifests(), rendered)
}

// TestClusterInfoCountsServedNamespaces: a pattern in application.namespaces makes the informers
// cluster-wide, and an Application of a namespace the controller does not serve is not counted, though
// its project permits that namespace.
func TestClusterInfoCountsServedNamespaces(t *testing.T) {
	s := stack.Start(t, stack.Options{DedicatedDestinations: 1, ApplicationNamespaces: []string{"served-*"}, Controller: fastClusterInfo})
	d := s.Destination(2)
	ns := s.NewNamespace(t, d, "counted")
	repo := s.Git().NewRepo(t, "counted")
	repo.CommitFiles(t, "v1", map[string]string{"app/cm.yaml": configMap("cm", map[string]string{"v": "1"}, nil)})
	project := &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "anywhere", Namespace: s.Namespace},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos: []string{"*"}, SourceNamespaces: []string{"*"},
			Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
		},
	}
	require.NoError(t, s.Control().Client.Create(t.Context(), project))
	// The served one last: the cluster reports a state only after that one reached its cache, so a
	// round that writes the state sees both.
	for _, namespace := range []string{"unserved", "served"} {
		app := s.NewApp(namespace, repo, "app", d, ns)
		app.Namespace, app.Spec.Project = s.NewNamespace(t, s.Control(), namespace), project.Name
		require.NoError(t, s.Control().Client.Create(t.Context(), app))
	}
	c := waitCluster(t, s, d, "the status of the cluster", func(c *apiclient.RegisteredCluster) bool {
		return connectionIs(c, v1alpha1.ConnectionStatusSuccessful)
	})
	assert.Equal(t, int64(1), c.Status.Info.ApplicationsCount)
}
