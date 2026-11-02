package core_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const (
	testNamespace = "patrol"
	testPassword  = "Password1!"
	serverKey     = "0123456789abcdef0123456789abcdef-server-key"
)

// passwordHash is computed once (bcrypt is slow).
var passwordHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(testPassword)
	if err != nil {
		panic(err)
	}
	return h
})

// clock is a settable time source shared by the token service, the login throttle and
// the handlers.
type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.ns.Store(time.Now().Truncate(time.Second).UnixNano())
	return c
}

func (c *clock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *clock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// settingsHolder rebuilds the settings snapshot from raw ConfigMap and Secret data.
type settingsHolder struct {
	mu     sync.Mutex
	cm     map[string]string
	rbacCM map[string]string
	secret map[string]string
	cur    atomic.Pointer[settings.Settings]
}

func (h *settingsHolder) Current() *settings.Settings { return h.cur.Load() }

func (h *settingsHolder) rebuild() {
	in := settings.Input{
		Namespace:         testNamespace,
		IgnorePartOfLabel: true,
		ConfigMaps: []*corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: testNamespace}, Data: maps.Clone(h.cm)},
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultRBACConfigMapName, Namespace: testNamespace}, Data: maps.Clone(h.rbacCM)},
		},
		Secrets: []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Namespace: testNamespace}, Data: toBytes(h.secret)}},
	}
	h.cur.Store(settings.Parse(in))
}

func (h *settingsHolder) setSecret(data map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.secret = maps.Clone(data)
	h.rebuild()
}

func (h *settingsHolder) setCM(fn func(cm map[string]string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fn(h.cm)
	h.rebuild()
}

func toBytes(m map[string]string) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
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

// fakeRepo implements the repo-server client's ListPlugins.
type fakeRepo struct {
	reposerver.Client
	plugins []string
	err     error
}

func (f *fakeRepo) ListPlugins(context.Context) (*reposerver.PluginList, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := &reposerver.PluginList{}
	for _, p := range f.plugins {
		out.Items = append(out.Items, reposerver.PluginInfo{Name: p})
	}
	return out, nil
}

type envOptions struct {
	cm          map[string]string
	rbac        map[string]string
	secret      map[string]string
	disableAuth bool
	repo        reposerver.Client
	version     *core.VersionInfo
	sso         *core.SSOConfig
	logger      *slog.Logger
	// secretLag delays the settings snapshot after secret writes (patrol-secret informer
	// and settings debounce in production).
	secretLag time.Duration
}

// testEnv is a running API server with the core routes.
type testEnv struct {
	t        *testing.T
	url      string
	clock    *clock
	holder   *settingsHolder
	secrets  *core.MemorySecretStore
	tokens   *auth.TokenService
	login    *auth.Authenticator
	enforcer *rbac.Enforcer
	audit    *auditLog
	registry *prometheus.Registry
	srv      *httptest.Server
}

func defaultCM() map[string]string {
	return map[string]string{
		"accounts.alice":         "login, apiKey",
		"accounts.ci":            "apiKey",
		"accounts.dave":          "login",
		"accounts.bob":           "login",
		"accounts.bob.enabled":   "false",
		"users.session.duration": "1h",
	}
}

func defaultSecret() map[string]string {
	return map[string]string{
		auth.KeyServerSecretKey:   serverKey,
		auth.KeyAdminPassword:     passwordHash(),
		"accounts.alice.password": passwordHash(),
		"accounts.dave.password":  passwordHash(),
		"accounts.bob.password":   passwordHash(),
	}
}

func defaultRBAC() map[string]string {
	return map[string]string{
		"policy.csv": "g, alice, role:readonly\ng, admins, role:admin\n",
	}
}

func newEnv(t *testing.T, o envOptions) *testEnv {
	t.Helper()
	if o.cm == nil {
		o.cm = defaultCM()
	}
	if o.rbac == nil {
		o.rbac = defaultRBAC()
	}
	if o.secret == nil {
		o.secret = defaultSecret()
	}
	e := &testEnv{t: t, clock: newClock(), audit: &auditLog{}, registry: prometheus.NewRegistry()}
	e.srv = httptest.NewUnstartedServer(nil)
	e.url = "http://" + e.srv.Listener.Addr().String()
	if _, ok := o.cm["url"]; !ok {
		o.cm["url"] = e.url
	}
	e.holder = &settingsHolder{cm: maps.Clone(o.cm), rbacCM: maps.Clone(o.rbac), secret: maps.Clone(o.secret)}
	e.holder.rebuild()
	e.secrets = core.NewMemorySecretStore(o.secret)
	e.secrets.OnUpdate = e.holder.setSecret
	if o.secretLag > 0 {
		e.secrets.OnUpdate = func(data map[string]string) {
			time.AfterFunc(o.secretLag, func() { e.holder.setSecret(data) })
		}
	}

	lookup := auth.AccountLookupFunc(func(_ context.Context, name string) (auth.Account, error) {
		return e.holder.Current().Accounts().Get(name)
	})
	ks, err := e.holder.Current().SigningKeys()
	require.NoError(t, err)
	var sso *core.SSO
	var external auth.ExternalVerifier
	if o.sso != nil {
		sso, err = core.NewSSO(*o.sso)
		require.NoError(t, err)
		external = sso.ExternalVerifier(e.holder)
	}
	e.tokens, err = auth.NewTokenService(auth.TokenServiceConfig{
		External: external,
		Keys:     ks, Accounts: lookup, Revocations: auth.NewMemoryRevocationStore(auth.RevocationLimits{}, e.clock.Now), Now: e.clock.Now,
		Projects: auth.TokenRegistryFunc(func(context.Context, string, string, int64) (bool, error) { return true, nil }),
	})
	require.NoError(t, err)
	lc := auth.DefaultLoginConfig()
	lc.MaxFailures, lc.DelayMin, lc.DelayMax = 3, 0, 0
	e.login, err = auth.NewAuthenticator(lookup, lc, auth.WithLoginClock(e.clock.Now))
	require.NoError(t, err)
	e.enforcer = rbac.NewEnforcer()
	require.NoError(t, e.enforcer.ApplyConfigMap(rbac.BuiltinPolicy(), o.rbac))

	m, err := rest.NewMetrics(e.registry)
	require.NoError(t, err)
	h := core.New(core.Deps{
		Config:     core.Config{Namespace: testNamespace, DisableAuth: o.disableAuth, AppsInAnyNamespaceEnabled: true},
		Settings:   e.holder,
		Tokens:     e.tokens,
		Login:      e.login,
		Secrets:    e.secrets,
		SSO:        sso,
		RepoServer: o.repo,
		Version:    o.version,
		Metrics:    m,
		Logger:     o.logger,
		Now:        e.clock.Now,
	})
	tbl := rest.NewTable()
	h.Register(tbl)
	require.NoError(t, tbl.Err())
	env := &rest.Env{
		Authenticator: &rest.Authenticator{
			Verifier: e.tokens,
			Policy: func() rest.AuthPolicy {
				st := e.holder.Current()
				sc := st.RBAC().Scopes
				if len(sc) == 0 {
					sc = rbac.DefaultScopes()
				}
				return rest.AuthPolicy{AnonymousEnabled: st.AnonymousUserEnabled(), Scopes: sc}
			},
			Cookie:      auth.CookieOptions{Path: "/"},
			DisableAuth: o.disableAuth,
		},
		Enforcer:    e.enforcer,
		Auditor:     e.audit,
		Metrics:     m,
		DisableAuth: o.disableAuth,
		CSRF:        rest.CSRFConfig{TrustedOrigins: func() []string { return []string{e.url} }},
	}
	handler, err := tbl.Handler(env)
	require.NoError(t, err)
	e.srv.Config.Handler = rest.Chain(handler, rest.Recover(nil, m), rest.RequestIDs())
	e.srv.Start()
	t.Cleanup(e.srv.Close)
	return e
}

// client returns an API client, logged in when user is not empty.
func (e *testEnv) client(user string) *apiclient.Client {
	e.t.Helper()
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: e.url})
	require.NoError(e.t, err)
	if user != "" {
		_, err := c.Login(context.Background(), user, testPassword)
		require.NoError(e.t, err)
	}
	return c
}

// clientWithToken returns a client using tok.
func (e *testEnv) clientWithToken(tok string) *apiclient.Client {
	e.t.Helper()
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: e.url, AuthToken: tok})
	require.NoError(e.t, err)
	return c
}

// browser is an HTTP client with a cookie jar that does not follow redirects.
func (e *testEnv) browser() *http.Client {
	e.t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(e.t, err)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (e *testEnv) loginCount(method, result string) float64 {
	e.t.Helper()
	mfs, err := e.registry.Gather()
	require.NoError(e.t, err)
	for _, mf := range mfs {
		if mf.GetName() != "patrol_server_login_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["method"] == method && labels["result"] == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func apiErr(t *testing.T, err error) *apiclient.APIError {
	t.Helper()
	require.Error(t, err)
	var ae *apiclient.APIError
	require.True(t, errors.As(err, &ae), "want *apiclient.APIError, got %T: %v", err, err)
	return ae
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return string(b)
}

// fakeIdP is a minimal OpenID provider: discovery, JWKS, an authorization endpoint that
// redirects back with a code, a token endpoint enforcing client authentication,
// redirect_uri and PKCE, and a userinfo endpoint.
type fakeIdP struct {
	t        *testing.T
	srv      *httptest.Server
	key      *rsa.PrivateKey
	clientID string
	secret   string
	cliID    string

	mu       sync.Mutex
	codes    map[string]idpCode
	access   map[string]string
	claims   map[string]any
	extraJWK map[string]any // served beside the signing key
}

type idpCode struct{ redirectURI, nonce, challenge string }

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &fakeIdP{t: t, key: key, clientID: "patrol-web", secret: "s3cr3t-client", cliID: "patrol-cli-app",
		codes: map[string]idpCode{}, access: map[string]string{},
		claims: map[string]any{"email": "jane@example.com", "name": "Jane", "groups": []any{"devs", "admins"}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/auth", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/keys", "userinfo_endpoint": f.srv.URL + "/userinfo",
			"response_types_supported": []string{"code"}, "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, _ *http.Request) {
		keys := []map[string]any{{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "k1",
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(f.key.E)).Bytes()),
		}}
		f.mu.Lock()
		if f.extraJWK != nil {
			keys = append(keys, f.extraJWK)
		}
		f.mu.Unlock()
		writeJSON(w, map[string]any{"keys": keys})
	})
	mux.HandleFunc("GET /auth", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != f.clientID {
			http.Error(w, "bad client", http.StatusBadRequest)
			return
		}
		code := "code-" + randString(t)
		f.mu.Lock()
		f.codes[code] = idpCode{redirectURI: q.Get("redirect_uri"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
		f.mu.Unlock()
		cb, _ := url.Parse(q.Get("redirect_uri"))
		cq := cb.Query()
		cq.Set("code", code)
		cq.Set("state", q.Get("state"))
		cb.RawQuery = cq.Encode()
		http.Redirect(w, r, cb.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, sec, ok := r.BasicAuth()
		if !ok {
			id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		f.mu.Lock()
		c, found := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		if id != f.clientID || sec != f.secret || !found || c.redirectURI != r.PostForm.Get("redirect_uri") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		if c.challenge != "" {
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		access := "at-" + randString(t)
		f.mu.Lock()
		f.access[access] = "user-123"
		f.mu.Unlock()
		writeJSON(w, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600,
			"id_token": f.idToken(map[string]any{"nonce": c.nonce, "aud": f.clientID})})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		sub, ok := f.access[tok]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"sub": sub, "groups": []string{"from-userinfo"}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// idToken signs an ID token for user-123; override replaces claims (nil deletes).
func (f *fakeIdP) idToken(override map[string]any) string {
	now := time.Now()
	claims := jwt.MapClaims{"iss": f.srv.URL, "sub": "user-123", "aud": f.clientID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	f.mu.Lock()
	for k, v := range f.claims {
		claims[k] = v
	}
	f.mu.Unlock()
	for k, v := range override {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(f.key)
	require.NoError(f.t, err)
	return s
}

// accessToken registers an access token for user-123 (userinfo).
func (f *fakeIdP) accessToken() string {
	tok := "at-" + randString(f.t)
	f.mu.Lock()
	f.access[tok] = "user-123"
	f.mu.Unlock()
	return tok
}

func (f *fakeIdP) oidcConfig(extra string) string {
	return "name: Fake\nissuer: " + f.srv.URL + "\nclientID: " + f.clientID + "\nclientSecret: " + f.secret +
		"\ncliClientID: " + f.cliID + "\nrequestedScopes: [openid, profile, email, groups]\n" + extra
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randString(t *testing.T) string {
	b := make([]byte, 12)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(b)
}
