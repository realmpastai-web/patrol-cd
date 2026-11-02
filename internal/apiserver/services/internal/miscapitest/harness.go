// Package miscapitest is the end-to-end test harness of the ApplicationSet, notification,
// badge and proxy extension services: a real route chain (internal/apiserver/rest) with the
// real auth.TokenService, rbac.Enforcer and settings snapshots, served over HTTP and called
// through pkg/apiclient, against an envtest API server with the Patrol CRDs.
//
// It is imported only by tests.
package miscapitest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	apirest "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/envtestx"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// ServerKey is the JWT signing key of test environments (not a real secret).
const ServerKey = "0123456789abcdef0123456789abcdef-test-signing-key"

var testCfg *rest.Config

// Main runs m with a shared envtest API server (Patrol CRDs from crdDir) when
// KUBEBUILDER_ASSETS is set; tests that need it call Config, which skips otherwise.
// PATROL_REQUIRE_ENVTEST=1 turns a missing KUBEBUILDER_ASSETS into a failure.
func Main(m *testing.M, crdDir string) int {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("PATROL_REQUIRE_ENVTEST") == "1" {
			fmt.Fprintln(os.Stderr, "PATROL_REQUIRE_ENVTEST=1 but KUBEBUILDER_ASSETS is not set")
			return 1
		}
		return m.Run()
	}
	ctrllog.SetLogger(logr.Discard())
	env := &envtest.Environment{CRDDirectoryPaths: []string{crdDir}, ErrorIfCRDPathMissing: true}
	cfg, err := envtestx.Start(env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start envtest: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "stop envtest: %v\n", err)
		}
	}()
	testCfg = cfg
	return m.Run()
}

// CRDDir returns deploy/crds relative to a package directory depth levels below the
// repository root.
func CRDDir(depth int) string {
	parts := make([]string, 0, depth+2)
	for range depth {
		parts = append(parts, "..")
	}
	return filepath.Join(append(parts, "deploy", "crds")...)
}

// Config returns the envtest configuration, skipping the test without envtest.
func Config(t testing.TB) *rest.Config {
	t.Helper()
	if testCfg == nil {
		t.Skip("KUBEBUILDER_ASSETS is not set: envtest integration test skipped")
	}
	return rest.CopyConfig(testCfg)
}

// Scheme returns a scheme with the core and Patrol types.
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}

// Unique returns prefix plus a random suffix (namespace and object names).
func Unique(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// Settings rebuilds a settings snapshot from raw ConfigMap and Secret data.
type Settings struct {
	mu              sync.Mutex
	namespace       string
	cm, rbacCM      map[string]string
	secret          map[string]string
	notificationsCM map[string]string
	notifSecret     map[string]string
	cur             atomic.Pointer[settings.Settings]
}

// Current implements the services' SettingsSource.
func (s *Settings) Current() *settings.Settings { return s.cur.Load() }

func (s *Settings) rebuild() {
	in := settings.Input{
		Namespace:         s.namespace,
		IgnorePartOfLabel: true,
		ConfigMaps: []*corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: s.namespace}, Data: maps.Clone(s.cm)},
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultRBACConfigMapName, Namespace: s.namespace}, Data: maps.Clone(s.rbacCM)},
		},
		Secrets: []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Namespace: s.namespace}, Data: toBytes(s.secret)}},
	}
	if s.notificationsCM != nil {
		in.ConfigMaps = append(in.ConfigMaps, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultNotificationsConfigMapName, Namespace: s.namespace}, Data: maps.Clone(s.notificationsCM)})
	}
	if s.notifSecret != nil {
		in.Secrets = append(in.Secrets, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultNotificationsSecretName, Namespace: s.namespace}, Data: toBytes(s.notifSecret)})
	}
	s.cur.Store(settings.Parse(in))
}

// UpdateCM changes patrol-cm.
func (s *Settings) UpdateCM(fn func(cm map[string]string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.cm)
	s.rebuild()
}

// UpdateSecret changes patrol-secret.
func (s *Settings) UpdateSecret(fn func(sec map[string]string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s.secret)
	s.rebuild()
}

// SetNotifications replaces patrol-notifications-cm and its Secret (nil: absent).
func (s *Settings) SetNotifications(cm, secret map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notificationsCM, s.notifSecret = maps.Clone(cm), maps.Clone(secret)
	s.rebuild()
}

func toBytes(m map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
}

// AuditLog collects audit entries.
type AuditLog struct {
	mu      sync.Mutex
	entries []apirest.AuditEntry
}

// Audit implements rest.Auditor.
func (a *AuditLog) Audit(_ context.Context, e apirest.AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entries = append(a.entries, e)
}

// ByOperation returns the entries of an operation.
func (a *AuditLog) ByOperation(op string) []apirest.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []apirest.AuditEntry
	for _, e := range a.entries {
		if e.Operation == op {
			out = append(out, e)
		}
	}
	return out
}

// Recorder collects Kubernetes Events recorded by services.
type Recorder struct {
	mu     sync.Mutex
	Events []string // "<type> <reason> <namespace>/<name> <message>"
}

// Eventf implements rest.EventRecorder.
func (r *Recorder) Eventf(obj runtime.Object, eventtype, reason, messageFmt string, args ...any) {
	ref, _ := obj.(*corev1.ObjectReference)
	name := ""
	if ref != nil {
		name = ref.Namespace + "/" + ref.Name
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Events = append(r.Events, fmt.Sprintf("%s %s %s %s", eventtype, reason, name, fmt.Sprintf(messageFmt, args...)))
}

// All returns the recorded events.
func (r *Recorder) All() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.Events...)
}

// Registrar registers routes (apiserver.Service).
type Registrar interface{ Register(t *apirest.Table) }

// Options configures New.
type Options struct {
	// CM, RBAC and Secret are patrol-cm, patrol-rbac-cm and patrol-secret data; the signing
	// key is added to Secret.
	CM, RBAC, Secret map[string]string
	// Kube is false for services that need no Kubernetes API (no envtest required).
	Kube bool
}

// Env is a running API server with the services under test.
type Env struct {
	T         *testing.T
	URL       string
	Namespace string
	Kube      client.WithWatch
	Settings  *Settings
	Tokens    *auth.TokenService
	Enforcer  *rbac.Enforcer
	Audit     *AuditLog
	Recorder  *Recorder
	Registry  *prometheus.Registry
	srv       *httptest.Server
}

// New starts an API server whose routes are those of the registrars build returns. The
// control-plane namespace is unique per environment (and created when Kube is set).
func New(t *testing.T, o Options, build func(env *Env) []Registrar) *Env {
	t.Helper()
	ns := Unique("patrol")
	e := &Env{T: t, Namespace: ns, Audit: &AuditLog{}, Recorder: &Recorder{}, Registry: prometheus.NewRegistry()}
	if o.Kube {
		cfg := Config(t)
		k, err := client.NewWithWatch(cfg, client.Options{Scheme: Scheme()})
		require.NoError(t, err)
		e.Kube = k
		e.CreateNamespace(ns)
	}
	sec := maps.Clone(o.Secret)
	if sec == nil {
		sec = map[string]string{}
	}
	sec[auth.KeyServerSecretKey] = ServerKey
	cm := maps.Clone(o.CM)
	if cm == nil {
		cm = map[string]string{}
	}
	rbacCM := maps.Clone(o.RBAC)
	if rbacCM == nil {
		rbacCM = map[string]string{}
	}
	e.Settings = &Settings{namespace: ns, cm: cm, rbacCM: rbacCM, secret: sec}
	e.Settings.rebuild()

	lookup := auth.AccountLookupFunc(func(_ context.Context, name string) (auth.Account, error) {
		return e.Settings.Current().Accounts().Get(name)
	})
	ks, err := e.Settings.Current().SigningKeys()
	require.NoError(t, err)
	e.Tokens, err = auth.NewTokenService(auth.TokenServiceConfig{
		Keys: ks, Accounts: lookup, Revocations: auth.NewMemoryRevocationStore(auth.RevocationLimits{}, nil),
		Projects: auth.TokenRegistryFunc(func(context.Context, string, string, int64) (bool, error) { return true, nil }),
	})
	require.NoError(t, err)
	e.Enforcer = rbac.NewEnforcer()
	require.NoError(t, e.Enforcer.ApplyConfigMap(rbac.BuiltinPolicy(), rbacCM))

	m, err := apirest.NewMetrics(e.Registry)
	require.NoError(t, err)
	tbl := apirest.NewTable()
	for _, r := range build(e) {
		r.Register(tbl)
	}
	require.NoError(t, tbl.Err())
	e.srv = httptest.NewUnstartedServer(nil)
	e.URL = "http://" + e.srv.Listener.Addr().String()
	env := &apirest.Env{
		Authenticator: &apirest.Authenticator{
			Verifier: e.Tokens,
			Policy: func() apirest.AuthPolicy {
				st := e.Settings.Current()
				sc := st.RBAC().Scopes
				if len(sc) == 0 {
					sc = rbac.DefaultScopes()
				}
				return apirest.AuthPolicy{AnonymousEnabled: st.AnonymousUserEnabled(), Scopes: sc}
			},
		},
		Enforcer: e.Enforcer,
		Auditor:  e.Audit,
		Metrics:  m,
		CSRF:     apirest.CSRFConfig{TrustedOrigins: func() []string { return []string{e.URL} }},
	}
	handler, err := tbl.Handler(env)
	require.NoError(t, err)
	e.srv.Config.Handler = apirest.Chain(handler, apirest.Recover(nil, m), apirest.RequestIDs(), apirest.MaxBody(64<<20))
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	return e
}

// SetRBAC replaces patrol-rbac-cm in the enforcer.
func (e *Env) SetRBAC(data map[string]string) {
	e.T.Helper()
	require.NoError(e.T, e.Enforcer.ApplyConfigMap(rbac.BuiltinPolicy(), data))
}

// SetProjectRoles loads the roles of a project into the enforcer (as the server does from
// AppProject.spec.roles).
func (e *Env) SetProjectRoles(project string, roles ...rbac.Role) {
	e.T.Helper()
	require.NoError(e.T, e.Enforcer.SetProjectPolicies(project, roles))
}

// CreateNamespace creates a namespace (envtest has no namespace controller; never deleted).
func (e *Env) CreateNamespace(name string) {
	e.T.Helper()
	err := e.Kube.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	require.NoError(e.T, err)
}

// ClientWithToken returns an API client using tok ("" for none).
func (e *Env) ClientWithToken(tok string) *apiclient.Client {
	e.T.Helper()
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: e.URL, AuthToken: tok, Timeout: time.Minute})
	require.NoError(e.T, err)
	return c
}

// Anonymous returns a client without credentials.
func (e *Env) Anonymous() *apiclient.Client { return e.ClientWithToken("") }

// LoginToken issues a login session token for a local account of patrol-cm.
func (e *Env) LoginToken(user string) string {
	e.T.Helper()
	acct, err := e.Settings.Current().Accounts().Get(user)
	require.NoError(e.T, err)
	iss, err := e.Tokens.IssueLoginSession(context.Background(), acct, time.Hour)
	require.NoError(e.T, err)
	return iss.Token
}

// Login returns a client logged in as a local account.
func (e *Env) Login(user string) *apiclient.Client { return e.ClientWithToken(e.LoginToken(user)) }

// ProjectClient returns a client with a project role token.
func (e *Env) ProjectClient(project, role string) *apiclient.Client {
	e.T.Helper()
	iss, err := e.Tokens.IssueProjectToken(context.Background(), project, role, "", time.Hour)
	require.NoError(e.T, err)
	return e.ClientWithToken(iss.Token)
}

// SSOToken issues an SSO session for subject with groups.
func (e *Env) SSOToken(subject, email string, groups ...string) string {
	e.T.Helper()
	iss, err := e.Tokens.IssueSSOSession(context.Background(), auth.SSOIdentity{Subject: subject, Email: email, Groups: groups}, time.Hour)
	require.NoError(e.T, err)
	return iss.Token
}

// Accounts returns patrol-cm entries enabling login for users.
func Accounts(users ...string) map[string]string {
	out := map[string]string{}
	for _, u := range users {
		out["accounts."+u] = "login"
	}
	return out
}

// Policy joins policy.csv lines.
func Policy(lines ...string) map[string]string {
	return map[string]string{"policy.csv": strings.Join(lines, "\n") + "\n"}
}

// Ctx returns a test context bounded to two minutes.
func Ctx(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}
