package applications_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8srest "k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// resourcePolicy extends the application policy with the logs/exec and fine-grained grants
// the resource routes need: alice everything in proj-a (applications, logs, exec); reader
// get in proj-a; logger get + logs; execer get + exec; patcher update of Deployments only
// (fine-grained); updater plain update; podkiller delete of Pods only (fine-grained); actor
// actions on Deployments; bob everything in proj-b.
const resourcePolicy = `p, role:a-admin, applications, *, proj-a/*, allow
p, role:a-admin, logs, get, proj-a/*, allow
p, role:a-admin, exec, create, proj-a/*, allow
p, role:a-reader, applications, get, proj-a/*, allow
p, role:a-logger, applications, get, proj-a/*, allow
p, role:a-logger, logs, get, proj-a/*, allow
p, role:a-exec, applications, get, proj-a/*, allow
p, role:a-exec, exec, create, proj-a/*, allow
p, role:a-patcher, applications, get, proj-a/*, allow
p, role:a-patcher, applications, update/apps/Deployment/*, proj-a/*, allow
p, role:a-updater, applications, get, proj-a/*, allow
p, role:a-updater, applications, update, proj-a/*, allow
p, role:a-podkiller, applications, get, proj-a/*, allow
p, role:a-podkiller, applications, delete/*/Pod/*/*, proj-a/*, allow
p, role:a-actor, applications, get, proj-a/*, allow
p, role:a-actor, applications, action/apps/Deployment/*, proj-a/*, allow
p, role:b-admin, applications, *, proj-b/*, allow
p, role:b-admin, logs, get, proj-b/*, allow
p, role:b-admin, exec, create, proj-b/*, allow
g, alice, role:a-admin
g, reader, role:a-reader
g, logger, role:a-logger
g, execer, role:a-exec
g, patcher, role:a-patcher
g, updater, role:a-updater
g, podkiller, role:a-podkiller
g, actor, role:a-actor
g, bob, role:b-admin
g, admin, role:admin
`

const internalToken = "test-internal-token-0123456789"

// fakeState is a treeapi.StateProvider over fixed trees and managed resources: the real
// tree API handler and client run in front of it.
type fakeState struct {
	mu      sync.Mutex
	trees   map[treeapi.AppKey]*compare.ApplicationTree
	managed map[treeapi.AppKey][]compare.ManagedResource
	missing map[treeapi.AppKey]bool
	calls   atomic.Int64
	// ssdCalls counts server-side diff (dry run) calls.
	ssdCalls atomic.Int64
}

func newFakeState() *fakeState {
	return &fakeState{trees: map[treeapi.AppKey]*compare.ApplicationTree{}, managed: map[treeapi.AppKey][]compare.ManagedResource{}, missing: map[treeapi.AppKey]bool{}}
}

func (f *fakeState) setTree(k treeapi.AppKey, t *compare.ApplicationTree) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.trees[k] = t
	delete(f.missing, k)
}

func (f *fakeState) setMissing(k treeapi.AppKey, missing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missing[k] = missing
}

func (f *fakeState) Tree(_ context.Context, k treeapi.AppKey) (*compare.ApplicationTree, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing[k] {
		return nil, fmt.Errorf("%w: not compared yet", treeapi.ErrUnavailable)
	}
	t, ok := f.trees[k]
	if !ok {
		return nil, fmt.Errorf("%w: application %s", treeapi.ErrNotFound, k)
	}
	b, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	out := &compare.ApplicationTree{}
	return out, json.Unmarshal(b, out)
}

func (f *fakeState) ManagedResources(_ context.Context, k treeapi.AppKey) ([]compare.ManagedResource, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing[k] {
		return nil, fmt.Errorf("%w: not compared yet", treeapi.ErrUnavailable)
	}
	return f.managed[k], nil
}

func (f *fakeState) ServerSideDiff(_ context.Context, _ treeapi.AppKey, live, target []*unstructured.Unstructured) ([]diff.Result, error) {
	f.ssdCalls.Add(1)
	out := make([]diff.Result, len(live))
	for i := range live {
		enc := func(u *unstructured.Unstructured) []byte {
			if u == nil {
				return []byte("null")
			}
			b, _ := json.Marshal(u.Object)
			return b
		}
		out[i] = diff.Result{NormalizedLive: enc(live[i]), PredictedLive: enc(target[i]), Modified: !bytes.Equal(enc(live[i]), enc(target[i]))}
	}
	return out, nil
}

func (f *fakeState) LiveResource(context.Context, treeapi.AppKey, compare.ResourceRef) (*unstructured.Unstructured, error) {
	return nil, treeapi.ErrNotFound
}

func (f *fakeState) SensitiveAnnotations() []string { return []string{"token"} }

// seenRequest is a request the fake destination cluster received.
type seenRequest struct {
	method, path, query, impersonate string
	impersonateGroup                 bool
	body                             string
}

// fakePodLog configures the log of one pod.
type fakePodLog struct {
	lines []string // "<RFC3339Nano> <text>"
	// follow keeps writing "tick <n>" lines while the client follows.
	follow bool
	// status answers with this HTTP status and message instead (e.g. a container not running).
	status  int
	message string
	// hold, when set, delays the response (status and lines) until it is closed or the client
	// gives up: a kubelet that answers late.
	hold <-chan struct{}
	// served, when set, is called after the lines were written and flushed.
	served func()
}

// fakeCluster is a destination cluster: requests are proxied to envtest (with envtest's
// admin credentials, forwarding impersonation headers, so Kubernetes RBAC applies to an
// impersonated service account), except pods/log and pods/exec, which envtest cannot serve
// without a kubelet and which this kubelet stand-in answers.
type fakeCluster struct {
	t     *testing.T
	srv   *httptest.Server
	proxy *httputil.ReverseProxy

	mu        sync.Mutex
	requests  []seenRequest
	logs      map[string]fakePodLog // "ns/pod"
	shells    map[string]bool       // shells present in containers
	execs     []url.Values
	resizes   []string
	followEnd chan string // pods whose follow stream ended (client gone)
}

var (
	logPathRE  = regexp.MustCompile(`^/api/v1/namespaces/([^/]+)/pods/([^/]+)/log$`)
	execPathRE = regexp.MustCompile(`^/api/v1/namespaces/([^/]+)/pods/([^/]+)/exec$`)
)

func newFakeCluster(t *testing.T, cfg *k8srest.Config) *fakeCluster {
	t.Helper()
	upstream, err := url.Parse(cfg.Host)
	require.NoError(t, err)
	tr, err := k8srest.TransportFor(cfg)
	require.NoError(t, err)
	f := &fakeCluster{t: t, logs: map[string]fakePodLog{}, shells: map[string]bool{"sh": true}, followEnd: make(chan string, 16)}
	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Host = upstream.Host
		},
		Transport: tr,
	}
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCluster) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sr := seenRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, impersonate: r.Header.Get("Impersonate-User"),
		impersonateGroup: r.Header.Get("Impersonate-Group") != "" || r.Header.Get("Impersonate-Uid") != ""}
	if r.Method == http.MethodDelete || r.Method == http.MethodPatch || r.Method == http.MethodPost {
		b, _ := io.ReadAll(r.Body)
		sr.body = string(b)
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	f.mu.Lock()
	f.requests = append(f.requests, sr)
	f.mu.Unlock()
	if m := logPathRE.FindStringSubmatch(r.URL.Path); m != nil && r.Method == http.MethodGet {
		f.serveLog(w, r, m[1]+"/"+m[2])
		return
	}
	if m := execPathRE.FindStringSubmatch(r.URL.Path); m != nil {
		f.serveExec(w, r)
		return
	}
	f.proxy.ServeHTTP(w, r)
}

func writeStatus(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"}, Status: metav1.StatusFailure, Code: int32(code), Message: msg, Reason: metav1.StatusReasonBadRequest})
}

func (f *fakeCluster) serveLog(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	pl, ok := f.logs[key]
	f.mu.Unlock()
	if !ok {
		writeStatus(w, http.StatusNotFound, fmt.Sprintf("pods %q not found", key))
		return
	}
	if pl.status != 0 {
		writeStatus(w, pl.status, pl.message)
		return
	}
	if r.URL.Query().Get("timestamps") != "true" {
		writeStatus(w, http.StatusBadRequest, "timestamps expected")
		return
	}
	if pl.hold != nil {
		select {
		case <-pl.hold:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	for _, l := range pl.lines {
		_, _ = io.WriteString(w, l+"\n")
	}
	w.(http.Flusher).Flush()
	if pl.served != nil {
		pl.served()
	}
	if !pl.follow || r.URL.Query().Get("follow") != "true" {
		return
	}
	for i := 0; ; i++ {
		select {
		case <-r.Context().Done():
			f.followEnd <- key
			return
		case <-time.After(20 * time.Millisecond):
			_, _ = fmt.Fprintf(w, "%s tick %d\n", time.Now().UTC().Format(time.RFC3339Nano), i)
			w.(http.Flusher).Flush()
		}
	}
}

// serveExec is a container exec over the Kubernetes WebSocket channel protocol: the shell
// must exist; stdin is echoed as "echo:<data>", resizes are acknowledged, "exit\r" ends
// with a success status.
func (f *fakeCluster) serveExec(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	f.execs = append(f.execs, q)
	shellOK := f.shells[q.Get("command")]
	f.mu.Unlock()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"v5.channel.k8s.io", "v4.channel.k8s.io"}})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx := r.Context()
	send := func(ch byte, p []byte) error {
		return conn.Write(ctx, websocket.MessageBinary, append([]byte{ch}, p...))
	}
	status := func(st metav1.Status) {
		b, _ := json.Marshal(st)
		_ = send(3, b)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	if !shellOK {
		status(metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonInternalError,
			Message: fmt.Sprintf("exec: %q: executable file not found in $PATH", q.Get("command"))})
		return
	}
	_ = send(1, []byte(q.Get("command")+"$ "))
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil || typ != websocket.MessageBinary || len(data) == 0 {
			return
		}
		switch data[0] {
		case 0:
			in := string(data[1:])
			if in == "exit\r" {
				status(metav1.Status{Status: metav1.StatusSuccess})
				return
			}
			_ = send(1, []byte("echo:"+in))
		case 4:
			var sz struct{ Width, Height uint16 }
			_ = json.Unmarshal(data[1:], &sz)
			f.mu.Lock()
			f.resizes = append(f.resizes, fmt.Sprintf("%dx%d", sz.Width, sz.Height))
			f.mu.Unlock()
			_ = send(1, []byte(fmt.Sprintf("resized %dx%d", sz.Width, sz.Height)))
		}
	}
}

func (f *fakeCluster) seen(match func(seenRequest) bool) []seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []seenRequest
	for _, r := range f.requests {
		if match(r) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeCluster) setLog(key string, pl fakePodLog) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs[key] = pl
}

// updateLog changes the log configuration of one pod.
func (f *fakeCluster) updateLog(key string, update func(*fakePodLog)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pl := f.logs[key]
	update(&pl)
	f.logs[key] = pl
}

// clusterConfigs returns the fake cluster's configuration for every destination.
type clusterConfigs struct {
	host  string
	err   error // what RESTConfig fails with, when set
	calls atomic.Int64
}

func (c *clusterConfigs) RESTConfig(_ context.Context, cl *v1alpha1.Cluster) (*k8srest.Config, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	if cl == nil || cl.Spec.Server == "" {
		return nil, fmt.Errorf("no cluster")
	}
	return &k8srest.Config{Host: c.host}, nil
}

type resEnvOptions struct {
	cm        map[string]string
	resources func(*applications.ResourceDeps)
	now       func() time.Time // the service's clock (Deps.Now); nil: time.Now
	// reader wraps what the service reads through (Deps.Reader); nil: the Kubernetes client.
	reader func(client.Reader) client.Reader
}

// resEnv is an API server with the resource routes: envtest is both the control plane and
// (through fakeCluster) the destination cluster; the controller state comes from the real
// tree API handler and client over fakeState.
type resEnv struct {
	*testEnv
	state    *fakeState
	cluster  *fakeCluster
	configs  *clusterConfigs
	sessions *auditLog
	url      string
	appNS    string // destination namespace of the test Application
	objs     map[string]client.Object
}

func newResEnv(t *testing.T, o resEnvOptions) *resEnv {
	t.Helper()
	e := newEnv(t, envOptions{cm: o.cm, policy: resourcePolicy})
	cfg := envConfig(t)
	re := &resEnv{testEnv: e, state: newFakeState(), sessions: &auditLog{}, appNS: e.nsA, objs: map[string]client.Object{}}
	re.cluster = newFakeCluster(t, cfg)
	re.configs = &clusterConfigs{host: re.cluster.srv.URL}

	th, err := treeapi.NewHandler(re.state, treeapi.HandlerOptions{Token: internalToken})
	require.NoError(t, err)
	tsrv := httptest.NewServer(th)
	t.Cleanup(tsrv.Close)
	tc, err := treeapi.NewClient(treeapi.StaticRouter{URL: tsrv.URL}, internalToken)
	require.NoError(t, err)

	rd := applications.ResourceDeps{
		Tree: tc, Clusters: re.configs, Auditor: re.sessions,
		TreePollInterval: 100 * time.Millisecond,
		// The merge window is an hour, so the merged order of several pods depends on their lines
		// alone: a line goes out once every open stream has one buffered or has ended. With a short
		// window, a pod whose log answered later than one window was merged after the others (issue
		// #223). TestMergeLogs covers the window's flush. A multi-pod follow test in this harness
		// would wait until every pod has written a line.
		Logs:     applications.LogsConfig{MergeWindow: time.Hour},
		Terminal: applications.TerminalConfig{PingInterval: time.Second},
	}
	if o.resources != nil {
		o.resources(&rd)
	}
	deps := applications.Deps{
		Config: applications.Config{Namespace: e.ns, RefreshTimeout: 10 * time.Second, RefreshPollInterval: 100 * time.Millisecond},
		Client: e.kube, Settings: e.holder, Events: e.broadcaster, Repositories: e.repos, RepoServer: e.repo, Resources: rd,
		Now: o.now,
	}
	if o.reader != nil {
		deps.Reader = o.reader(e.kube)
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
	re.url = srv.URL
	return re
}

// rclient returns a client of the resource server authenticated as an SSO user.
func (re *resEnv) rclient(user string) *apiclient.Client {
	re.t.Helper()
	c, _ := re.rclientToken(user)
	return c
}

func (re *resEnv) rclientToken(user string) (*apiclient.Client, string) {
	re.t.Helper()
	opts := &apiclient.ClientOptions{ServerAddr: re.url}
	if user != "" {
		iss, err := re.tokens.IssueSSOSession(re.ctx, auth.SSOIdentity{Subject: user}, time.Hour)
		require.NoError(re.t, err)
		opts.AuthToken = iss.Token
	}
	c, err := apiclient.NewClient(opts)
	require.NoError(re.t, err)
	return c, opts.AuthToken
}

// setPolicy replaces the RBAC policy (e.g. to revoke a permission mid-stream).
func (re *resEnv) setPolicy(policy string) {
	re.t.Helper()
	require.NoError(re.t, re.enforcer.ApplyConfigMap(rbac.BuiltinPolicy(), map[string]string{"policy.csv": policy}))
}

func (re *resEnv) create(obj client.Object) client.Object {
	re.t.Helper()
	key := obj.GetObjectKind().GroupVersionKind().Kind + "/" + obj.GetName()
	require.NoError(re.t, re.kube.Create(re.ctx, obj))
	re.objs[key] = obj
	return obj
}

func refOf(obj client.Object, group, version, kind string) compare.ResourceRef {
	return compare.ResourceRef{Group: group, Version: version, Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: string(obj.GetUID())}
}

func toUnstr(t *testing.T, obj any, apiVersion, kind string) *unstructured.Unstructured {
	t.Helper()
	b, err := json.Marshal(obj)
	require.NoError(t, err)
	u := &unstructured.Unstructured{}
	require.NoError(t, json.Unmarshal(b, &u.Object))
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)
	return u
}

// fixture creates the destination objects of the test Application "guestbook" (project
// proj-a, destination namespace nsA) in envtest and registers its tree and managed
// resources: Deployment web → ReplicaSet web-abc → Pods web-abc-1 (running) and web-abc-2
// (container not running); ConfigMap cfg; Secret creds; CronJob nightly; orphaned
// ConfigMap stray; Pod outsider (in the namespace, not in the tree).
func (re *resEnv) fixture(appName, proj string) *v1alpha1.Application {
	re.t.Helper()
	ns := re.appNS
	if _, ok := re.objs["ServiceAccount/default"]; !ok {
		re.create(&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns}})
	}
	if _, ok := re.objs["Deployment/web"]; !ok {
		re.createObjects()
	}
	dep, rs, p1, p2 := re.objs["Deployment/web"], re.objs["ReplicaSet/web-abc"], re.objs["Pod/web-abc-1"], re.objs["Pod/web-abc-2"]
	cm, secret, cron, stray := re.objs["ConfigMap/cfg"], re.objs["Secret/creds"], re.objs["CronJob/nightly"], re.objs["ConfigMap/stray"]

	app := re.newApp(appName, proj)
	app.Spec.Destination.Namespace = ns
	app = re.createApp(app)
	depRef := refOf(dep, "apps", "v1", "Deployment")
	rsRef := refOf(rs, "apps", "v1", "ReplicaSet")
	tree := &compare.ApplicationTree{
		Nodes: []compare.ResourceNode{
			{ResourceRef: depRef},
			{ResourceRef: rsRef, ParentRefs: []compare.ResourceRef{depRef}},
			{ResourceRef: refOf(p1, "", "v1", "Pod"), ParentRefs: []compare.ResourceRef{rsRef}, Info: []compare.InfoItem{{Name: "Status Reason", Value: "Running"}}},
			{ResourceRef: refOf(p2, "", "v1", "Pod"), ParentRefs: []compare.ResourceRef{rsRef}},
			{ResourceRef: refOf(cm, "", "v1", "ConfigMap")},
			{ResourceRef: refOf(secret, "", "v1", "Secret")},
			{ResourceRef: refOf(cron, "batch", "v1", "CronJob")},
		},
		OrphanedNodes: []compare.ResourceNode{{ResourceRef: refOf(stray, "", "v1", "ConfigMap")}},
	}
	key := treeapi.AppKey{Namespace: app.Namespace, Name: app.Name}
	re.state.setTree(key, tree)
	secretU := toUnstr(re.t, secret, "v1", "Secret")
	targetSecret := secretU.DeepCopy()
	_ = unstructured.SetNestedField(targetSecret.Object, "bmV3", "data", "password")
	re.state.mu.Lock()
	re.state.managed[key] = []compare.ManagedResource{
		{Key: compare.ResourceKey{Group: "apps", Kind: "Deployment", Namespace: ns, Name: "web"}, Version: "v1",
			Target: toUnstr(re.t, dep, "apps/v1", "Deployment"), Live: toUnstr(re.t, dep, "apps/v1", "Deployment")},
		{Key: compare.ResourceKey{Kind: "ConfigMap", Namespace: ns, Name: "cfg"}, Version: "v1", Target: toUnstr(re.t, cm, "v1", "ConfigMap"), Live: toUnstr(re.t, cm, "v1", "ConfigMap")},
		{Key: compare.ResourceKey{Kind: "Secret", Namespace: ns, Name: "creds"}, Version: "v1", Target: targetSecret, Live: secretU,
			Diff: diff.Result{Modified: true, NormalizedLive: mustJSON(re.t, secretU.Object), PredictedLive: mustJSON(re.t, targetSecret.Object)}},
		{Key: compare.ResourceKey{Kind: "Pod", Namespace: ns, Name: "hook"}, Version: "v1", Hook: true},
	}
	re.state.mu.Unlock()
	re.cluster.setLog(ns+"/web-abc-1", fakePodLog{lines: []string{
		"2026-01-01T00:00:01Z web1 started", "2026-01-01T00:00:03Z GET /healthz 200", "2026-01-01T00:00:05Z web1 ERROR boom",
	}})
	re.cluster.setLog(ns+"/web-abc-2", fakePodLog{lines: []string{
		"2026-01-01T00:00:02Z web2 started", "2026-01-01T00:00:04Z GET /healthz 200",
	}})
	return app
}

// createObjects creates the destination objects of fixture once per environment.
func (re *resEnv) createObjects() {
	re.t.Helper()
	ns := re.appNS
	labels := map[string]string{"app": "web"}
	tmpl := corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{AutomountServiceAccountToken: ptr.To(false), Containers: []corev1.Container{{Name: "app", Image: "nginx"}}}}
	re.create(&appsv1.Deployment{TypeMeta: metav1.TypeMeta{Kind: "Deployment"}, ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(2)), Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: tmpl}})
	re.create(&appsv1.ReplicaSet{TypeMeta: metav1.TypeMeta{Kind: "ReplicaSet"}, ObjectMeta: metav1.ObjectMeta{Name: "web-abc", Namespace: ns},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: tmpl}})
	pod := func(name string, running bool) client.Object {
		p := &corev1.Pod{TypeMeta: metav1.TypeMeta{Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}, Spec: tmpl.Spec}
		re.create(p)
		st := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
		if running {
			st = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
		}
		p.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Image: "nginx", ImageID: "x", State: st}}}
		require.NoError(re.t, re.kube.Status().Update(re.ctx, p))
		return p
	}
	pod("web-abc-1", true)
	pod("web-abc-2", false)
	pod("outsider", true)
	re.create(&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "cfg", Namespace: ns}, Data: map[string]string{"k": "v"}})
	re.create(&corev1.Secret{TypeMeta: metav1.TypeMeta{Kind: "Secret"}, ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: ns,
		Annotations: map[string]string{"token": "t0p-secret"}}, Data: map[string][]byte{"password": []byte("hunter2")}})
	re.create(&batchv1.CronJob{TypeMeta: metav1.TypeMeta{Kind: "CronJob"}, ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: ns},
		Spec: batchv1.CronJobSpec{Schedule: "0 0 * * *", JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever, Containers: []corev1.Container{{Name: "c", Image: "busybox"}}}}}}}})
	re.create(&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: "stray", Namespace: ns}})

}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// grantServiceAccount creates the service account and a Role/RoleBinding granting verbs on
// resources in the destination namespace.
func (re *resEnv) grantServiceAccount(name string, rules ...rbacv1.PolicyRule) {
	re.t.Helper()
	ns := re.appNS
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := re.kube.Create(re.ctx, sa); err != nil {
		require.True(re.t, strings.Contains(err.Error(), "already exists"), err)
	}
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + randSuffix(re.t), Namespace: ns}, Rules: rules}
	require.NoError(re.t, re.kube.Create(re.ctx, role))
	require.NoError(re.t, re.kube.Create(re.ctx, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: ns},
		RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: ns}}}))
}

// eventually polls cond (envtest RBAC changes are not instantaneous).
func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
