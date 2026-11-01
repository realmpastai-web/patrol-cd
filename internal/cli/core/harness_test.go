package core_test

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // fake commit SHAs, not a security use
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli"
	"github.com/juliosaraiva/patrol-cd/internal/cli/app"
	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/cli/core"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver"
	reposerverapi "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

const (
	repoURL   = "https://github.com/org/core-demo"
	inCluster = v1alpha1.KubernetesInternalAPIServerAddr
	// harnessAgent is the User-Agent of the test's own (cluster-admin) clients, so audit
	// checks can tell them apart from the CLI's requests.
	harnessAgent = "patrol-core-test-harness"
	// coreAgentSuffix ends the User-Agent of every request of the in-process server.
	coreAgentSuffix = "(core mode)"
	testToken       = "core-test-internal-token-0123456789"
)

// coreEnv is one test's Patrol namespace in envtest with an HTTP repo-server fake and the
// real tree API handler (both on loopback, reached through the Endpoints override that
// replaces the Kubernetes port-forward), a controller stand-in, and CLI runs in core mode.
type coreEnv struct {
	t      *testing.T
	ctx    context.Context
	ns     string
	dest   string
	admin  client.Client
	kube   kubernetes.Interface
	dyn    dynamic.Interface
	mapper meta.RESTMapper
	repo   *repoFake
	opts   core.Options
	runner *clitest.Runner

	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	errs   []error
}

func newCoreEnv(t *testing.T) *coreEnv {
	t.Helper()
	if adminCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set: envtest core mode test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	cfg := rest.CopyConfig(adminCfg)
	cfg.UserAgent = harnessAgent
	admin, err := client.New(cfg, client.Options{Scheme: clitest.Scheme})
	require.NoError(t, err)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	e := &coreEnv{
		t: t, ctx: ctx, admin: admin, kube: kube, dyn: dyn, repo: newRepoFake(),
		mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(kube.Discovery())),
	}
	for _, p := range []*string{&e.ns, &e.dest} {
		ns, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "patrol-core-"}}, metav1.CreateOptions{})
		require.NoError(t, err)
		*p = ns.Name
	}
	partOf := map[string]string{settings.LabelKeyPartOf: settings.LabelValuePartOf}
	// Patrol RBAC grants nothing: core mode must not enforce it.
	for _, cm := range []*corev1.ConfigMap{
		{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Labels: partOf}, Data: map[string]string{"url": "https://patrol.example.com"}},
		{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultRBACConfigMapName, Labels: partOf}, Data: map[string]string{"policy.default": "", "policy.csv": ""}},
	} {
		_, err := kube.CoreV1().ConfigMaps(e.ns).Create(ctx, cm, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	_, err = kube.CoreV1().Secrets(e.ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "patrol-internal-token"}, Data: map[string][]byte{"token": []byte(testToken)},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, admin.Create(ctx, &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: "default"},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:  []string{"*"},
			Destinations: []v1alpha1.ApplicationDestination{{Server: "*", Namespace: "*"}},
		},
	}))

	// The repo-server and the controller tree API, as HTTP services with the internal token.
	rh, err := reposerver.NewHandler(e.repo, reposerver.HandlerOptions{Token: testToken, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	repoSrv := httptest.NewServer(rh)
	t.Cleanup(repoSrv.Close)
	th, err := treeapi.NewHandler(&treeState{e: e}, treeapi.HandlerOptions{Token: testToken})
	require.NoError(t, err)
	treeSrv := httptest.NewServer(th)
	t.Cleanup(treeSrv.Close)
	// The token is read from Secret patrol-internal-token with the caller's credentials.
	e.opts = core.Options{Endpoints: &core.Endpoints{RepoServer: repoSrv.URL, Controller: treeSrv.URL}}
	e.runner = clitest.NewRunner(t, app.Register, cli.Registrar(core.Registrar(e.opts)))

	sctx, stop := context.WithCancel(ctx)
	e.cancel, e.done = stop, make(chan struct{})
	go e.runController(sctx)
	t.Cleanup(func() {
		e.cancel()
		<-e.done
		if t.Failed() {
			e.mu.Lock()
			for _, err := range e.errs {
				t.Logf("controller stand-in: %v", err)
			}
			e.mu.Unlock()
		}
	})
	return e
}

// user creates an envtest user with the given rules in the Patrol namespace (Role +
// RoleBinding) and returns the path of a kubeconfig whose context namespace is it.
func (e *coreEnv) user(name string, rules ...rbacv1.PolicyRule) (path, username string) {
	e.t.Helper()
	username = name + "-" + strings.TrimPrefix(e.ns, "patrol-core-")
	u, err := testEnv.AddUser(envtest.User{Name: username}, nil)
	require.NoError(e.t, err)
	if len(rules) > 0 {
		role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: name}, Rules: rules}
		require.NoError(e.t, e.admin.Create(e.ctx, role))
		require.NoError(e.t, e.admin.Create(e.ctx, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: name},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
			Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: rbacv1.UserKind, Name: username}},
		}))
	}
	raw, err := u.KubeConfig()
	require.NoError(e.t, err)
	kc, err := clientcmd.Load(raw)
	require.NoError(e.t, err)
	for _, c := range kc.Contexts {
		c.Namespace = e.ns
	}
	path = filepath.Join(e.t.TempDir(), "kubeconfig")
	require.NoError(e.t, clientcmd.WriteToFile(*kc, path))
	require.NoError(e.t, os.Chmod(path, 0o600))
	return path, username
}

// operatorRules let a user manage everything of Patrol in the namespace with plain
// Kubernetes RBAC (no impersonate verb).
func operatorRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"*"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps", "secrets"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "list"}},
		{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get"}},
	}
}

// run runs `patrol --core args...` with kubeconfig.
func (e *coreEnv) run(kubeconfig string, args ...string) clitest.Result {
	e.t.Helper()
	return e.runner.RunWith(clitest.RunOptions{Env: map[string]string{"KUBECONFIG": kubeconfig}, Timeout: 3 * time.Minute},
		append([]string{"--core"}, args...)...)
}

func (e *coreEnv) app(name string) *v1alpha1.Application {
	a := &v1alpha1.Application{}
	if err := e.admin.Get(e.ctx, client.ObjectKey{Namespace: e.ns, Name: name}, a); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		e.t.Fatal(err)
	}
	return a
}

func (e *coreEnv) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (e *coreEnv) noteErr(err error) {
	if err == nil || errors.Is(err, context.Canceled) || apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.errs) < 20 {
		e.errs = append(e.errs, err)
	}
}

// ---------------------------------------------------------------------------------------
// Repo-server fake (served over HTTP by the real repo-server handler)

func fakeSHA(rev string) string {
	if rev == "" {
		rev = "HEAD"
	}
	sum := sha1.Sum([]byte(rev)) //nolint:gosec // fake commit SHA
	return hex.EncodeToString(sum[:])
}

type repoFake struct {
	*clitest.FakeRepoServer
	mu        sync.Mutex
	manifests map[string][]string
	rendered  *reposerverapi.ManifestRequest // the last render
}

func newRepoFake() *repoFake {
	return &repoFake{FakeRepoServer: &clitest.FakeRepoServer{}, manifests: map[string][]string{}}
}

func (r *repoFake) set(t *testing.T, path string, objs ...string) {
	t.Helper()
	var out []string
	for _, o := range objs {
		j, err := yaml.YAMLToJSON([]byte(o))
		require.NoError(t, err)
		out = append(out, string(j))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.manifests[path] = out
}

func (r *repoFake) manifestsFor(path string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.manifests[path]...)
}

func (r *repoFake) lastRender() *reposerverapi.ManifestRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rendered
}

func (r *repoFake) GenerateManifest(_ context.Context, req *reposerverapi.ManifestRequest) (*reposerverapi.ManifestResponse, error) {
	r.mu.Lock()
	r.rendered = req
	r.mu.Unlock()
	return &reposerverapi.ManifestResponse{Manifests: r.manifestsFor(req.Source.Path), Revision: fakeSHA(req.Revision), SourceType: "Directory"}, nil
}

func (r *repoFake) ResolveRevision(_ context.Context, req *reposerverapi.ResolveRevisionRequest) (*reposerverapi.ResolveRevisionResponse, error) {
	return &reposerverapi.ResolveRevisionResponse{Revision: fakeSHA(req.Revision), AmbiguousRevision: req.Revision}, nil
}

func (r *repoFake) GetAppDetails(context.Context, *reposerverapi.AppDetailsQuery) (*reposerverapi.AppDetails, error) {
	return &reposerverapi.AppDetails{Type: "Directory", Directory: &reposerverapi.DirectoryAppSpec{}}, nil
}

func (r *repoFake) GetRevisionMetadata(_ context.Context, req *reposerverapi.RevisionMetadataRequest) (*reposerverapi.RevisionMetadata, error) {
	return &reposerverapi.RevisionMetadata{Author: "Dev <dev@example.com>", Message: "commit " + req.Revision, Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}

// ---------------------------------------------------------------------------------------
// Desired and live state, tree API state provider

type managed struct {
	key    compare.ResourceKey
	ver    string
	target *unstructured.Unstructured
	live   *unstructured.Unstructured
}

func sourceOf(a *v1alpha1.Application) v1alpha1.ApplicationSource {
	if s := a.Spec.GetSources(); len(s) > 0 {
		return s[0]
	}
	return v1alpha1.ApplicationSource{}
}

func (e *coreEnv) managed(ctx context.Context, a *v1alpha1.Application) ([]managed, error) {
	var out []managed
	for _, m := range e.repo.manifestsFor(sourceOf(a).Path) {
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON([]byte(m)); err != nil {
			return nil, err
		}
		if u.GetNamespace() == "" {
			u.SetNamespace(a.Spec.Destination.Namespace)
		}
		live, gvk, err := e.live(ctx, u)
		if err != nil {
			return nil, err
		}
		out = append(out, managed{key: compare.ResourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: u.GetNamespace(), Name: u.GetName()}, ver: gvk.Version, target: u, live: live})
	}
	return out, nil
}

func (e *coreEnv) resourceFor(u *unstructured.Unstructured) (schema.GroupVersionResource, schema.GroupVersionKind, error) {
	gvk := u.GroupVersionKind()
	m, err := e.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return schema.GroupVersionResource{}, gvk, err
	}
	return m.Resource, gvk, nil
}

func (e *coreEnv) live(ctx context.Context, u *unstructured.Unstructured) (*unstructured.Unstructured, schema.GroupVersionKind, error) {
	gvr, gvk, err := e.resourceFor(u)
	if err != nil {
		return nil, gvk, err
	}
	live, err := e.dyn.Resource(gvr).Namespace(u.GetNamespace()).Get(ctx, u.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, gvk, nil
	}
	return live, gvk, err
}

func modified(target, live *unstructured.Unstructured) bool {
	if live == nil {
		return true
	}
	want, _ := target.Object["data"].(map[string]any)
	have, _ := live.Object["data"].(map[string]any)
	if len(want) != len(have) {
		return true
	}
	for k, v := range want {
		if fmt.Sprint(have[k]) != fmt.Sprint(v) {
			return true
		}
	}
	return false
}

func encode(u *unstructured.Unstructured) []byte {
	if u == nil {
		return []byte("null")
	}
	b, _ := json.Marshal(u.Object)
	return b
}

type treeState struct{ e *coreEnv }

func (s *treeState) appFor(ctx context.Context, k treeapi.AppKey) (*v1alpha1.Application, error) {
	a := &v1alpha1.Application{}
	if err := s.e.admin.Get(ctx, client.ObjectKey{Namespace: k.Namespace, Name: k.Name}, a); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: application %s", treeapi.ErrNotFound, k)
		}
		return nil, err
	}
	return a, nil
}

func (s *treeState) Tree(ctx context.Context, k treeapi.AppKey) (*compare.ApplicationTree, error) {
	a, err := s.appFor(ctx, k)
	if err != nil {
		return nil, err
	}
	ms, err := s.e.managed(ctx, a)
	if err != nil {
		return nil, err
	}
	tree := &compare.ApplicationTree{}
	for _, m := range ms {
		if m.live == nil {
			continue
		}
		tree.Nodes = append(tree.Nodes, compare.ResourceNode{
			ResourceRef: compare.ResourceRef{Group: m.key.Group, Version: m.ver, Kind: m.key.Kind, Namespace: m.key.Namespace, Name: m.key.Name, UID: string(m.live.GetUID())},
			Health:      &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy},
		})
	}
	return tree, nil
}

func (s *treeState) ManagedResources(ctx context.Context, k treeapi.AppKey) ([]compare.ManagedResource, error) {
	a, err := s.appFor(ctx, k)
	if err != nil {
		return nil, err
	}
	ms, err := s.e.managed(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]compare.ManagedResource, 0, len(ms))
	for _, m := range ms {
		mr := compare.ManagedResource{Key: m.key, Version: m.ver, Target: m.target, Live: m.live}
		mr.Diff = diff.Result{Modified: modified(m.target, m.live), PredictedLive: encode(m.target)}
		if m.live != nil {
			mr.Diff.NormalizedLive = encode(m.live)
		}
		out = append(out, mr)
	}
	return out, nil
}

func (s *treeState) ServerSideDiff(_ context.Context, _ treeapi.AppKey, live, target []*unstructured.Unstructured) ([]diff.Result, error) {
	out := make([]diff.Result, len(live))
	for i := range live {
		mod := target[i] == nil || modified(target[i], live[i])
		pred := encode(target[i])
		if !mod {
			pred = encode(live[i])
		}
		out[i] = diff.Result{NormalizedLive: encode(live[i]), PredictedLive: pred, Modified: mod}
	}
	return out, nil
}

func (s *treeState) LiveResource(ctx context.Context, _ treeapi.AppKey, ref compare.ResourceRef) (*unstructured.Unstructured, error) {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(schema.GroupVersionKind{Group: ref.Group, Version: ref.Version, Kind: ref.Kind})
	u.SetNamespace(ref.Namespace)
	u.SetName(ref.Name)
	live, _, err := s.e.live(ctx, u)
	if err != nil {
		return nil, err
	}
	if live == nil {
		return nil, treeapi.ErrNotFound
	}
	return live, nil
}

func (s *treeState) SensitiveAnnotations() []string { return nil }

// ---------------------------------------------------------------------------------------
// Controller stand-in: compares Applications, executes Operations, finishes deletions.

func (e *coreEnv) runController(ctx context.Context) {
	defer close(e.done)
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var ops v1alpha1.OperationList
		if err := e.admin.List(ctx, &ops, client.InNamespace(e.ns)); err != nil {
			e.noteErr(err)
			continue
		}
		for i := range ops.Items {
			e.noteErr(e.runOperation(ctx, &ops.Items[i]))
		}
		var apps v1alpha1.ApplicationList
		if err := e.admin.List(ctx, &apps, client.InNamespace(e.ns)); err != nil {
			e.noteErr(err)
			continue
		}
		for i := range apps.Items {
			e.noteErr(e.reconcile(ctx, &apps.Items[i], false))
		}
	}
}

func (e *coreEnv) reconcile(ctx context.Context, a *v1alpha1.Application, force bool) error {
	if a.DeletionTimestamp != nil {
		if len(a.Finalizers) == 0 {
			return nil
		}
		return e.admin.Patch(ctx, a, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`)))
	}
	_, refresh := a.Annotations[v1alpha1.AnnotationKeyRefresh]
	if !force && !refresh && a.Status.ReconciledAt != nil && a.Status.ObservedGeneration == a.Generation {
		return nil
	}
	ms, err := e.managed(ctx, a)
	if err != nil {
		return err
	}
	now := metav1.Now()
	st := &a.Status
	st.Resources = nil
	synced, healthy := true, true
	for _, m := range ms {
		rs := v1alpha1.ResourceStatus{Group: m.key.Group, Version: m.ver, Kind: m.key.Kind, Namespace: m.key.Namespace, Name: m.key.Name,
			Status: v1alpha1.SyncStatusCodeSynced, Health: &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusHealthy}}
		if modified(m.target, m.live) {
			rs.Status, synced = v1alpha1.SyncStatusCodeOutOfSync, false
		}
		if m.live == nil {
			rs.Health, healthy = &v1alpha1.HealthStatus{Status: v1alpha1.HealthStatusMissing}, false
		}
		st.Resources = append(st.Resources, rs)
	}
	st.Sync.Status = v1alpha1.SyncStatusCodeSynced
	if !synced {
		st.Sync.Status = v1alpha1.SyncStatusCodeOutOfSync
	}
	st.Sync.ComparedTo = v1alpha1.ComparedTo{Source: a.Spec.Source.DeepCopy(), Sources: a.Spec.Sources.DeepCopy(), Destination: a.Spec.Destination}
	health := v1alpha1.HealthStatusHealthy
	if !healthy {
		health = v1alpha1.HealthStatusMissing
	}
	if st.Health.Status != health {
		st.Health = v1alpha1.AppHealthStatus{Status: health, LastTransitionTime: &now}
	}
	st.SourceType = v1alpha1.ApplicationSourceTypeDirectory
	st.ReconciledAt = &now
	st.ObservedGeneration = a.Generation
	if err := e.admin.Status().Update(ctx, a); err != nil {
		return err
	}
	if refresh {
		return e.admin.Patch(ctx, a, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"`+v1alpha1.AnnotationKeyRefresh+`":null}}}`)))
	}
	return nil
}

func (e *coreEnv) runOperation(ctx context.Context, op *v1alpha1.Operation) error {
	if op.IsTerminal() {
		return nil
	}
	a := &v1alpha1.Application{}
	if err := e.admin.Get(ctx, client.ObjectKey{Namespace: op.Namespace, Name: op.Spec.Application}, a); err != nil {
		if apierrors.IsNotFound(err) {
			return e.finish(ctx, op, nil, v1alpha1.OperationFailed, "application not found", nil)
		}
		return err
	}
	if op.Spec.Sync == nil {
		return e.finish(ctx, op, a, v1alpha1.OperationError, "no sync operation", nil)
	}
	ms, err := e.managed(ctx, a)
	if err != nil {
		return e.finish(ctx, op, a, v1alpha1.OperationError, err.Error(), nil)
	}
	src := sourceOf(a)
	result := &v1alpha1.SyncOperationResult{Revision: fakeSHA(src.TargetRevision), Source: src.DeepCopy()}
	for _, m := range ms {
		gvr, _, err := e.resourceFor(m.target)
		if err != nil {
			return e.finish(ctx, op, a, v1alpha1.OperationError, err.Error(), nil)
		}
		verb := "configured"
		if m.live == nil {
			verb = "created"
		}
		if _, err := e.dyn.Resource(gvr).Namespace(m.target.GetNamespace()).Apply(ctx, m.target.GetName(), m.target, metav1.ApplyOptions{FieldManager: "patrol-standin", Force: true}); err != nil {
			return e.finish(ctx, op, a, v1alpha1.OperationFailed, err.Error(), result)
		}
		result.Resources = append(result.Resources, v1alpha1.ResourceResult{
			Group: m.key.Group, Version: m.ver, Kind: m.key.Kind, Namespace: m.key.Namespace, Name: m.key.Name,
			Status: v1alpha1.ResultCodeSynced, SyncPhase: v1alpha1.SyncPhaseSync, HookPhase: v1alpha1.OperationRunning,
			Message: strings.ToLower(m.key.Kind) + "/" + m.key.Name + " " + verb,
		})
	}
	return e.finish(ctx, op, a, v1alpha1.OperationSucceeded, "successfully synced (all tasks run)", result)
}

func (e *coreEnv) finish(ctx context.Context, op *v1alpha1.Operation, a *v1alpha1.Application, phase v1alpha1.OperationPhase, msg string, result *v1alpha1.SyncOperationResult) error {
	now := metav1.Now()
	started := op.Status.StartedAt
	if started == nil {
		started = &now
	}
	op.Status.Phase, op.Status.Message, op.Status.StartedAt, op.Status.FinishedAt, op.Status.SyncResult = phase, msg, started, &now, result
	op.Status.ObservedGeneration = op.Generation
	if err := e.admin.Status().Update(ctx, op); err != nil {
		return err
	}
	if a == nil {
		return nil
	}
	state := &v1alpha1.ApplicationOperationState{OperationName: op.Name, Phase: phase, Message: msg, StartedAt: started, FinishedAt: &now, InitiatedBy: op.Spec.InitiatedBy}
	if result != nil {
		state.SyncResult = &v1alpha1.SyncResultSummary{Revision: result.Revision}
	}
	a.Status.OperationState = state
	if phase == v1alpha1.OperationSucceeded && result != nil {
		a.Status.History = append(a.Status.History, v1alpha1.RevisionHistory{
			ID: int64(len(a.Status.History)), Revision: result.Revision, DeployedAt: now, DeployStartedAt: started, Source: result.Source,
			InitiatedBy: op.Spec.InitiatedBy, OperationName: op.Name,
		})
		a.Status.Sync.Revision = result.Revision
	}
	if err := e.admin.Status().Update(ctx, a); err != nil {
		return err
	}
	return e.reconcile(ctx, a, true)
}

// ---------------------------------------------------------------------------------------
// Audit log

// auditEvent is the part of an audit.k8s.io/v1 Event the tests check.
type auditEvent struct {
	Stage string `json:"stage"`
	Verb  string `json:"verb"`
	User  struct {
		Username string `json:"username"`
	} `json:"user"`
	ImpersonatedUser *struct {
		Username string `json:"username"`
	} `json:"impersonatedUser"`
	UserAgent string `json:"userAgent"`
	ObjectRef *struct {
		Resource  string `json:"resource"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		APIGroup  string `json:"apiGroup"`
	} `json:"objectRef"`
	ResponseStatus *struct {
		Code int `json:"code"`
	} `json:"responseStatus"`
}

// auditEvents returns the completed audit events of namespace ns (cluster-scoped events
// are included when ns is empty).
func auditEvents(t *testing.T, ns string) []auditEvent {
	t.Helper()
	f, err := os.Open(auditLogPath)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var out []auditEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev auditEvent
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Stage != "ResponseComplete" {
			continue
		}
		if ns != "" && (ev.ObjectRef == nil || ev.ObjectRef.Namespace != ns) {
			continue
		}
		out = append(out, ev)
	}
	require.NoError(t, sc.Err())
	return out
}
