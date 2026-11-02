package applications_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	k8srest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const (
	serverKey = "0123456789abcdef0123456789abcdef-apps-key"
	inCluster = "https://kubernetes.default.svc"
	repoURL   = "https://github.com/org/repo"
	// sha is a resolved commit returned by the fake repo-server.
	sha = "0123456789abcdef0123456789abcdef01234567"
)

// defaultPolicy grants: alice everything in proj-a; reader get in proj-a; syncer get and
// sync in proj-a; bob everything in proj-b; mover update in proj-a and create in proj-b;
// admin role:admin. "nobody" has no role.
const defaultPolicy = `p, role:a-admin, applications, *, proj-a/*, allow
p, role:a-reader, applications, get, proj-a/*, allow
p, role:a-syncer, applications, get, proj-a/*, allow
p, role:a-syncer, applications, sync, proj-a/*, allow
p, role:b-admin, applications, *, proj-b/*, allow
p, role:mover, applications, get, proj-a/*, allow
p, role:mover, applications, update, proj-a/*, allow
p, role:mover, applications, create, proj-b/*, allow
g, alice, role:a-admin
g, reader, role:a-reader
g, syncer, role:a-syncer
g, bob, role:b-admin
g, mover, role:mover
g, admin, role:admin
`

// settingsHolder builds settings snapshots from raw data.
type settingsHolder struct {
	mu     sync.Mutex
	ns     string
	cm     map[string]string
	rbacCM map[string]string
	cur    atomic.Pointer[settings.Settings]
}

func (h *settingsHolder) Current() *settings.Settings { return h.cur.Load() }

func (h *settingsHolder) rebuild() {
	in := settings.Input{
		Namespace:         h.ns,
		IgnorePartOfLabel: true,
		ConfigMaps: []*corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: h.ns}, Data: maps.Clone(h.cm)},
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultRBACConfigMapName, Namespace: h.ns}, Data: maps.Clone(h.rbacCM)},
		},
		Secrets: []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Namespace: h.ns},
			Data: map[string][]byte{auth.KeyServerSecretKey: []byte(serverKey)}}},
	}
	h.cur.Store(settings.Parse(in))
}

func (h *settingsHolder) setCM(key, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cm[key] = value
	h.rebuild()
}

// auditLog collects audit entries.
type auditLog struct {
	mu      sync.Mutex
	entries []rest.AuditEntry
}

func (a *auditLog) Audit(_ context.Context, e rest.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

func (a *auditLog) byOperation(op string) []rest.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []rest.AuditEntry
	for _, e := range a.entries {
		if e.Operation == op {
			out = append(out, e)
		}
	}
	return out
}

type multiAuditor []rest.Auditor

func (m multiAuditor) Audit(ctx context.Context, e rest.AuditEntry) {
	for _, a := range m {
		a.Audit(ctx, e)
	}
}

// fakeRepoServer implements applications.RepoServer and records requests.
type fakeRepoServer struct {
	mu          sync.Mutex
	manifests   map[string][]string // by source path
	genErr      error
	resolveErr  error
	resolved    map[string]string
	metadata    *reposerver.RevisionMetadata
	chart       *reposerver.ChartDetails
	oci         *reposerver.OCIMetadata
	generate    []*reposerver.ManifestRequest
	resolves    []*reposerver.ResolveRevisionRequest
	details     []*reposerver.AppDetailsQuery
	metaReqs    []*reposerver.RevisionMetadataRequest
	chartReqs   []*reposerver.ChartDetailsRequest
	filesBodies [][]byte
	// hook, when set, runs at the start of GenerateManifest (outside the lock): tests use
	// it to change the cluster while a handler validates.
	hook func(req *reposerver.ManifestRequest)
}

// setHook installs a GenerateManifest hook.
func (f *fakeRepoServer) setHook(h func(req *reposerver.ManifestRequest)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

func configMapJSON(name string) string {
	return `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"` + name + `"},"data":{"k":"v"}}`
}

func (f *fakeRepoServer) GenerateManifest(_ context.Context, req *reposerver.ManifestRequest) (*reposerver.ManifestResponse, error) {
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(req)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generate = append(f.generate, req)
	if f.genErr != nil {
		return nil, f.genErr
	}
	m, ok := f.manifests[req.Source.Path]
	if !ok {
		m = []string{configMapJSON("cm-" + strings.ReplaceAll(req.Source.Path, "/", "-"))}
	}
	return &reposerver.ManifestResponse{Manifests: m, Revision: "rev-" + req.Revision, SourceType: "Directory"}, nil
}

func (f *fakeRepoServer) GenerateManifestWithFiles(_ context.Context, req *reposerver.ManifestRequest, files io.Reader) (*reposerver.ManifestResponse, error) {
	b, err := io.ReadAll(files)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.generate = append(f.generate, req)
	f.filesBodies = append(f.filesBodies, b)
	if f.genErr != nil {
		return nil, f.genErr
	}
	return &reposerver.ManifestResponse{Manifests: f.manifests["local"], Revision: "", SourceType: "Directory"}, nil
}

func (f *fakeRepoServer) ResolveRevision(_ context.Context, req *reposerver.ResolveRevisionRequest) (*reposerver.ResolveRevisionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves = append(f.resolves, req)
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	rev := f.resolved[req.Revision]
	if rev == "" {
		rev = sha
	}
	return &reposerver.ResolveRevisionResponse{Revision: rev, AmbiguousRevision: req.Revision}, nil
}

func (f *fakeRepoServer) GetAppDetails(_ context.Context, req *reposerver.AppDetailsQuery) (*reposerver.AppDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.details = append(f.details, req)
	return &reposerver.AppDetails{Type: "Directory"}, nil
}

func (f *fakeRepoServer) GetRevisionMetadata(_ context.Context, req *reposerver.RevisionMetadataRequest) (*reposerver.RevisionMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.metaReqs = append(f.metaReqs, req)
	if f.metadata == nil {
		return nil, &reposerver.Error{Code: reposerver.CodeNotFound, Message: "revision not found"}
	}
	return f.metadata, nil
}

func (f *fakeRepoServer) GetRevisionChartDetails(_ context.Context, req *reposerver.ChartDetailsRequest) (*reposerver.ChartDetails, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chartReqs = append(f.chartReqs, req)
	if f.chart == nil {
		return nil, &reposerver.Error{Code: reposerver.CodeNotFound, Message: "chart not found"}
	}
	return f.chart, nil
}

func (f *fakeRepoServer) GetOCIMetadata(_ context.Context, req *reposerver.ChartDetailsRequest) (*reposerver.OCIMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chartReqs = append(f.chartReqs, req)
	if f.oci == nil {
		return nil, &reposerver.Error{Code: reposerver.CodeNotFound, Message: "artifact not found"}
	}
	return f.oci, nil
}

func (f *fakeRepoServer) lastGenerate() *reposerver.ManifestRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.generate) == 0 {
		return nil
	}
	return f.generate[len(f.generate)-1]
}

func (f *fakeRepoServer) lastResolve() *reposerver.ResolveRevisionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resolves) == 0 {
		return nil
	}
	return f.resolves[len(f.resolves)-1]
}

// fakeLive implements applications.LiveResources over a fixed set of objects.
type fakeLive struct {
	objs map[string]*unstructured.Unstructured // "group/kind/namespace/name"
}

func (f *fakeLive) GetManagedResource(_ context.Context, _ *v1alpha1.Application, _ *v1alpha1.AppProject, ref applications.ResourceRef) (*unstructured.Unstructured, error) {
	o, ok := f.objs[ref.Group+"/"+ref.Kind+"/"+ref.Namespace+"/"+ref.Name]
	if !ok {
		return nil, applications.ErrResourceNotInTree
	}
	return o.DeepCopy(), nil
}

type envOptions struct {
	cm       map[string]string
	policy   string
	config   func(e *testEnv, c *applications.Config)
	noEvents bool
	noLive   bool
	noRepo   bool
	// cachedReader serves reads from the informer cache, as in production.
	cachedReader bool
	// clientConfig adjusts the configuration of the handlers' Kubernetes client (e.g.
	// impersonating the server's service account); the test itself stays admin.
	clientConfig func(e *testEnv, cfg *k8srest.Config)
}

// testEnv is an API server with the application routes over envtest, with the real
// TokenService, RBAC enforcer, project lookups and repository resolver.
type testEnv struct {
	t           *testing.T
	ctx         context.Context
	ns          string // control plane
	nsA, nsB    string // tenant namespaces
	kube        client.Client
	holder      *settingsHolder
	tokens      *auth.TokenService
	enforcer    *rbac.Enforcer
	audit       *auditLog
	recorder    *record.FakeRecorder
	repo        *fakeRepoServer
	repos       *repository.Resolver
	live        *fakeLive
	broadcaster *applications.Broadcaster
	url         string
}

func randSuffix(t *testing.T) string {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

func newEnv(t *testing.T, o envOptions) *testEnv {
	t.Helper()
	cfg := envConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	base := "p" + randSuffix(t)
	e := &testEnv{t: t, ctx: ctx, ns: base + "-cp", nsA: base + "-team-a", nsB: base + "-team-b",
		audit: &auditLog{}, recorder: record.NewFakeRecorder(1000), repo: &fakeRepoServer{}, live: &fakeLive{objs: map[string]*unstructured.Unstructured{}}}
	var err error
	e.kube, err = client.New(cfg, client.Options{Scheme: testScheme})
	require.NoError(t, err)
	for _, ns := range []string{e.ns, e.nsA, e.nsB} {
		require.NoError(t, e.kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}

	cm := map[string]string{"url": "https://patrol.example.com"}
	maps.Copy(cm, o.cm)
	policy := o.policy
	if policy == "" {
		policy = defaultPolicy
	}
	e.holder = &settingsHolder{ns: e.ns, cm: cm, rbacCM: map[string]string{"policy.csv": policy}}
	e.holder.rebuild()

	ks, err := e.holder.Current().SigningKeys()
	require.NoError(t, err)
	e.tokens, err = auth.NewTokenService(auth.TokenServiceConfig{
		Keys: ks,
		Accounts: auth.AccountLookupFunc(func(_ context.Context, name string) (auth.Account, error) {
			return e.holder.Current().Accounts().Get(name)
		}),
		Revocations: auth.NewMemoryRevocationStore(auth.RevocationLimits{}, time.Now),
		Projects:    auth.TokenRegistryFunc(func(context.Context, string, string, int64) (bool, error) { return true, nil }),
	})
	require.NoError(t, err)
	e.enforcer = rbac.NewEnforcer()
	require.NoError(t, e.enforcer.ApplyConfigMap(rbac.BuiltinPolicy(), map[string]string{"policy.csv": policy}))

	// Application events: the production wiring (a Broadcaster on the cache's informer).
	var events applications.Events
	var reader client.Reader
	if !o.noEvents || o.cachedReader {
		cache, err := crcache.New(cfg, crcache.Options{Scheme: testScheme, DefaultNamespaces: map[string]crcache.Config{e.ns: {}, e.nsA: {}, e.nsB: {}}})
		require.NoError(t, err)
		inf, err := cache.GetInformer(ctx, &v1alpha1.Application{})
		require.NoError(t, err)
		if !o.noEvents {
			e.broadcaster = applications.NewBroadcaster(0)
			_, err = inf.AddEventHandler(e.broadcaster)
			require.NoError(t, err)
			events = e.broadcaster
		}
		go func() { _ = cache.Start(ctx) }()
		require.True(t, cache.WaitForCacheSync(ctx))
		if o.cachedReader {
			reader = cache
		}
	}

	// Repository credentials: the real resolver over Repository objects and Secrets.
	dyn, err := dynamic.NewForConfig(cfg)
	require.NoError(t, err)
	kc, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	lister, err := repository.NewLister(dyn, kc, repository.ListerOptions{Namespace: e.ns})
	require.NoError(t, err)
	require.NoError(t, lister.Start(ctx))
	e.repos, err = repository.NewResolver(lister, repository.Options{})
	require.NoError(t, err)

	conf := applications.Config{Namespace: e.ns, RefreshTimeout: 10 * time.Second, RefreshPollInterval: 100 * time.Millisecond}
	if o.config != nil {
		o.config(e, &conf)
	}
	handlerClient := e.kube
	if o.clientConfig != nil {
		hc := k8srest.CopyConfig(cfg)
		o.clientConfig(e, hc)
		handlerClient, err = client.New(hc, client.Options{Scheme: testScheme})
		require.NoError(t, err)
	}
	deps := applications.Deps{
		Config: conf, Client: handlerClient, Reader: reader, Settings: e.holder, Events: events, Repositories: e.repos,
		RepoServer: e.repo, LiveResources: e.live,
	}
	if o.noLive {
		deps.LiveResources = nil
	}
	if o.noRepo {
		deps.RepoServer = nil
	}
	svc := applications.New(deps)
	tbl := rest.NewTable()
	svc.Register(tbl)
	require.NoError(t, tbl.Err())
	m, err := rest.NewMetrics(prometheus.NewRegistry())
	require.NoError(t, err)
	env := &rest.Env{
		Authenticator: &rest.Authenticator{
			Verifier: e.tokens,
			Policy: func() rest.AuthPolicy {
				return rest.AuthPolicy{AnonymousEnabled: e.holder.Current().AnonymousUserEnabled(), Scopes: rbac.DefaultScopes()}
			},
		},
		Enforcer: e.enforcer,
		Auditor:  multiAuditor{e.audit, &rest.LogAuditor{Logger: slogDiscard(), Recorder: e.recorder}},
		Metrics:  m,
	}
	h, err := tbl.Handler(env)
	require.NoError(t, err)
	srv := httptest.NewServer(rest.Chain(h, rest.Recover(nil, m), rest.RequestIDs()))
	t.Cleanup(srv.Close)
	e.url = srv.URL

	e.createProject("proj-a", nil)
	e.createProject("proj-b", nil)
	return e
}

// client returns an API client authenticated as an SSO user (no token for "").
func (e *testEnv) client(user string, groups ...string) *apiclient.Client {
	e.t.Helper()
	opts := &apiclient.ClientOptions{ServerAddr: e.url}
	if user != "" {
		iss, err := e.tokens.IssueSSOSession(e.ctx, auth.SSOIdentity{Subject: user, Groups: groups}, time.Hour)
		require.NoError(e.t, err)
		opts.AuthToken = iss.Token
	}
	c, err := apiclient.NewClient(opts)
	require.NoError(e.t, err)
	return c
}

// projectClient returns a client with a project-role token.
func (e *testEnv) projectClient(proj, role string) *apiclient.Client {
	e.t.Helper()
	iss, err := e.tokens.IssueProjectToken(e.ctx, proj, role, "tok-"+randSuffix(e.t), time.Hour)
	require.NoError(e.t, err)
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: e.url, AuthToken: iss.Token})
	require.NoError(e.t, err)
	return c
}

// createProject creates an AppProject permitting the test repositories and every
// in-cluster namespace, and registers its role policies.
func (e *testEnv) createProject(name string, mutate func(*v1alpha1.AppProject)) *v1alpha1.AppProject {
	e.t.Helper()
	p := &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		Spec: v1alpha1.AppProjectSpec{
			SourceRepos:  []string{"https://github.com/org/*"},
			Destinations: []v1alpha1.ApplicationDestination{{Server: inCluster, Namespace: "*"}},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	require.NoError(e.t, e.kube.Create(e.ctx, p))
	require.NoError(e.t, e.enforcer.SetProjectPolicies(name, project.RBACRoles(p)))
	return p
}

func (e *testEnv) updateProject(name string, mutate func(*v1alpha1.AppProject)) {
	e.t.Helper()
	p := &v1alpha1.AppProject{}
	require.NoError(e.t, e.kube.Get(e.ctx, client.ObjectKey{Namespace: e.ns, Name: name}, p))
	mutate(p)
	require.NoError(e.t, e.kube.Update(e.ctx, p))
	require.NoError(e.t, e.enforcer.SetProjectPolicies(name, project.RBACRoles(p)))
}

// newApp returns an Application of project in the control-plane namespace.
func (e *testEnv) newApp(name, proj string) *v1alpha1.Application {
	return &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: e.ns},
		Spec: v1alpha1.ApplicationSpec{
			Project:     proj,
			Source:      &v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "guestbook", TargetRevision: "main"},
			Destination: v1alpha1.ApplicationDestination{Server: inCluster, Namespace: "default"},
		},
	}
}

// createApp stores app directly (as kubectl would).
func (e *testEnv) createApp(app *v1alpha1.Application) *v1alpha1.Application {
	e.t.Helper()
	app = app.DeepCopy()
	require.NoError(e.t, e.kube.Create(e.ctx, app))
	return app
}

func (e *testEnv) getApp(ns, name string) *v1alpha1.Application {
	e.t.Helper()
	a := &v1alpha1.Application{}
	require.NoError(e.t, e.kube.Get(e.ctx, client.ObjectKey{Namespace: ns, Name: name}, a))
	return a
}

// operations returns the Operations of an Application stored in ns.
func (e *testEnv) operations(ns, app string) []v1alpha1.Operation {
	e.t.Helper()
	list := &v1alpha1.OperationList{}
	require.NoError(e.t, e.kube.List(e.ctx, list, client.InNamespace(ns)))
	var out []v1alpha1.Operation
	for _, op := range list.Items {
		if op.Spec.Application == app {
			out = append(out, op)
		}
	}
	return out
}

// finishOperations marks every Operation of app terminal (as the controller would).
func (e *testEnv) finishOperations(ns, app string) {
	e.t.Helper()
	for _, op := range e.operations(ns, app) {
		op.Status.Phase = v1alpha1.OperationSucceeded
		require.NoError(e.t, e.kube.Status().Update(e.ctx, &op))
	}
}

// events drains the Kubernetes Events recorded so far.
func (e *testEnv) events() []string {
	var out []string
	for {
		select {
		case ev := <-e.recorder.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func apiErr(t *testing.T, err error) *apiclient.APIError {
	t.Helper()
	require.Error(t, err)
	var ae *apiclient.APIError
	require.True(t, errors.As(err, &ae), "want *apiclient.APIError, got %T: %v", err, err)
	return ae
}

// requireProblem asserts an API error's status and that its detail contains detail, and
// returns the problem document.
func requireProblem(t *testing.T, err error, status int, detail string) apiclient.Problem {
	t.Helper()
	ae := apiErr(t, err)
	require.Equal(t, status, ae.StatusCode, "detail: %s", ae.Problem.Detail)
	if detail != "" {
		require.Contains(t, ae.Problem.Detail, detail)
	}
	p := ae.Problem
	p.Code = ae.Code()
	return p
}

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func ptrBool(b bool) *bool { return &b }

// applicationsConfig shortens test option literals.
type applicationsConfig = applications.Config

func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }
