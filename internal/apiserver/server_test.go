package apiserver_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const repoRoot = "../.."

// TestOpenAPIUpToDate fails when api/openapi/openapi.json differs from the document
// generated from the route table. Regenerate with:
//
//	go run ./internal/apiserver/cmd/genopenapi
func TestOpenAPIUpToDate(t *testing.T) {
	t.Parallel()
	want, err := apiserver.OpenAPIDocument(repoRoot)
	require.NoError(t, err)
	again, err := apiserver.OpenAPIDocument(repoRoot)
	require.NoError(t, err)
	require.Equal(t, want, again, "generation must be deterministic")
	got, err := os.ReadFile(filepath.Join(repoRoot, apiserver.OpenAPIPath))
	require.NoError(t, err)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; run: go run ./internal/apiserver/cmd/genopenapi", apiserver.OpenAPIPath)
	}
}

// TestCoreRouteTable is the golden list of the session, account, settings and version
// routes of api-contract.md.
func TestCoreRouteTable(t *testing.T) {
	t.Parallel()
	tbl, err := apiserver.NewRouteTable()
	require.NoError(t, err)
	var got []string
	access := map[string]string{}
	for _, r := range tbl.Routes() {
		if r.Hidden {
			continue
		}
		got = append(got, r.Method+" "+r.Path+" "+r.OperationID)
		access[r.OperationID] = r.Access.Mode.String()
		if r.Access.Mode == rest.AccessRBAC {
			access[r.OperationID] += ":" + r.Access.Resource + "," + r.Access.Action
		}
	}
	assert.Equal(t, []string{
		"GET /api/v1/account listAccounts",
		"GET /api/v1/account/can-i/{resource}/{action}/{subresource...} canI",
		"PUT /api/v1/account/password updatePassword",
		"GET /api/v1/account/{name} getAccount",
		"POST /api/v1/account/{name}/token createAccountToken",
		"DELETE /api/v1/account/{name}/token/{id} deleteAccountToken",
		"GET /api/v1/auth/callback ssoCallback",
		"GET /api/v1/auth/login startSSOLogin",
		"DELETE /api/v1/session deleteSession",
		"POST /api/v1/session createSession",
		"POST /api/v1/session/oidc-exchange exchangeOIDCToken",
		"GET /api/v1/session/userinfo getUserInfo",
		"GET /api/v1/settings getSettings",
		"GET /api/v1/settings/plugins getPluginSettings",
		"GET /api/v1/version getVersion",
		"GET /api/version getVersionAlias",
	}, got)
	assert.Equal(t, map[string]string{
		"listAccounts": "authenticated", "canI": "authenticated", "updatePassword": "rbac:accounts,update",
		"getAccount": "rbac:accounts,get", "createAccountToken": "rbac:accounts,update", "deleteAccountToken": "rbac:accounts,update",
		"ssoCallback": "public", "startSSOLogin": "public", "deleteSession": "authenticated", "createSession": "public",
		"exchangeOIDCToken": "public", "getUserInfo": "public", "getSettings": "public", "getPluginSettings": "authenticated",
		"getVersion": "public", "getVersionAlias": "public",
	}, access)
}

// fakeSettings is a SettingsSource with subscriptions.
type fakeSettings struct {
	mu   sync.Mutex
	cur  atomic.Pointer[settings.Settings]
	subs []func(old, new *settings.Settings)
}

func (f *fakeSettings) Current() *settings.Settings { return f.cur.Load() }

func (f *fakeSettings) Subscribe(fn func(old, new *settings.Settings)) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, fn)
	return func() {}
}

func (f *fakeSettings) set(cm, rbacCM, secret map[string]string) {
	in := settings.Input{
		Namespace: "patrol", IgnorePartOfLabel: true,
		ConfigMaps: []*corev1.ConfigMap{
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName, Namespace: "patrol"}, Data: maps.Clone(cm)},
			{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultRBACConfigMapName, Namespace: "patrol"}, Data: maps.Clone(rbacCM)},
		},
		Secrets: []*corev1.Secret{{ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultSecretName, Namespace: "patrol"}, Data: bytesMap(secret)}},
	}
	n := settings.Parse(in)
	old := f.cur.Swap(n)
	f.mu.Lock()
	subs := slices.Clone(f.subs)
	f.mu.Unlock()
	for _, s := range subs {
		s(old, n)
	}
}

func bytesMap(m map[string]string) map[string][]byte {
	out := map[string][]byte{}
	for k, v := range m {
		out[k] = []byte(v)
	}
	return out
}

var adminHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword("Password1!")
	if err != nil {
		panic(err)
	}
	return h
})

type fixture struct {
	srv      *apiserver.Server
	settings *fakeSettings
	tokens   *auth.TokenService
	cm       map[string]string
	secret   map[string]string
}

func newFixture(t *testing.T, cfg apiserver.Config, mutate ...func(*apiserver.Deps)) *fixture {
	t.Helper()
	f := &fixture{
		settings: &fakeSettings{},
		cm:       map[string]string{"url": "https://patrol.example.com", "accounts.dave": "login"},
		secret:   map[string]string{auth.KeyServerSecretKey: strings.Repeat("s", 32), auth.KeyAdminPassword: adminHash(), "accounts.dave.password": adminHash()},
	}
	f.settings.set(f.cm, map[string]string{}, f.secret)
	lookup := apiserver.AccountLookup(f.settings)
	ks, err := f.settings.Current().SigningKeys()
	require.NoError(t, err)
	f.tokens, err = auth.NewTokenService(auth.TokenServiceConfig{Keys: ks, Accounts: lookup, Revocations: auth.NewMemoryRevocationStore(auth.RevocationLimits{}, nil)})
	require.NoError(t, err)
	lc := auth.DefaultLoginConfig()
	lc.DelayMin, lc.DelayMax = 0, 0
	login, err := auth.NewAuthenticator(lookup, lc)
	require.NoError(t, err)
	deps := apiserver.Deps{
		Settings: f.settings, Tokens: f.tokens, Login: login, Enforcer: rbac.NewEnforcer(),
		Registry: prometheus.NewRegistry(),
	}
	for _, m := range mutate {
		m(&deps)
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "patrol"
	}
	f.srv, err = apiserver.New(cfg, deps)
	require.NoError(t, err)
	return f
}

func uiFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte(`<!doctype html><html><head><base href="/" /><title>Patrol</title></head><body></body></html>`)},
		"assets/index-AbC123.js": {Data: []byte("console.log(1)")},
		"favicon.svg":            {Data: []byte("<svg/>")},
		".secret":                {Data: []byte("nope")},
	}
}

func serve(t *testing.T, h http.Handler, method, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://patrol.test"+path, nil)
	require.NoError(t, err)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestHandlerRootPathBaseHRefAndUI(t *testing.T) {
	t.Parallel()
	f := newFixture(t, apiserver.Config{Insecure: true, RootPath: "patrol", UI: uiFS()})
	h := f.srv.Handler()
	html := map[string]string{"Accept": "text/html,application/xhtml+xml"}

	resp := serve(t, h, http.MethodGet, "/patrol/api/version", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, readAll(t, resp), `"version":`)
	assert.Equal(t, http.StatusNotFound, serve(t, h, http.MethodGet, "/api/version", nil).StatusCode)
	assert.Equal(t, http.StatusOK, serve(t, h, http.MethodGet, "/healthz", nil).StatusCode)
	assert.Equal(t, http.StatusOK, serve(t, h, http.MethodGet, "/patrol/healthz", nil).StatusCode)
	assert.Equal(t, http.StatusMovedPermanently, serve(t, h, http.MethodGet, "/patrol", nil).StatusCode)

	resp = serve(t, h, http.MethodGet, "/patrol/applications/guestbook", html)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body := readAll(t, resp)
	assert.Contains(t, body, `<base href="/patrol/" />`, "basehref defaults to the root path")
	assert.Contains(t, resp.Header.Get("Cache-Control"), "no-cache")
	assert.Equal(t, "sameorigin", resp.Header.Get("X-Frame-Options"))
	assert.Contains(t, resp.Header.Get("Content-Security-Policy"), "default-src 'self'")
	assert.NotEmpty(t, resp.Header.Get(rest.RequestIDHeader))

	resp = serve(t, h, http.MethodGet, "/patrol/index.html", nil)
	assert.Contains(t, readAll(t, resp), `<base href="/patrol/" />`)

	resp = serve(t, h, http.MethodGet, "/patrol/assets/index-AbC123.js", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "public, max-age=31536000, immutable", resp.Header.Get("Cache-Control"))
	resp = serve(t, h, http.MethodGet, "/patrol/favicon.svg", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))

	assert.Equal(t, http.StatusNotFound, serve(t, h, http.MethodGet, "/patrol/assets/missing.js", nil).StatusCode, "non-HTML misses are 404")
	assert.Equal(t, http.StatusOK, serve(t, h, http.MethodGet, "/patrol/.secret", html).StatusCode, "dot-files are never served: the SPA index answers")
	assert.NotContains(t, readAll(t, serve(t, h, http.MethodGet, "/patrol/.secret", html)), "nope")

	resp = serve(t, h, http.MethodGet, "/patrol/api/v1/nope", html)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Equal(t, apiclient.ProblemContentType, resp.Header.Get("Content-Type"), "API misses never fall back to the UI")
}

func TestHandlerBaseHRefOverrideAndHeaders(t *testing.T) {
	t.Parallel()
	none := ""
	f := newFixture(t, apiserver.Config{Insecure: true, BaseHRef: "/ui/", UI: uiFS(), XFrameOptions: &none, ContentSecurityPolicy: &none})
	resp := serve(t, f.srv.Handler(), http.MethodGet, "/", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, readAll(t, resp), `<base href="/ui/" />`)
	assert.Empty(t, resp.Header.Get("X-Frame-Options"), "empty values disable the headers")
	assert.Empty(t, resp.Header.Get("Content-Security-Policy"))

	f = newFixture(t, apiserver.Config{Insecure: true})
	resp = serve(t, f.srv.Handler(), http.MethodGet, "/applications", map[string]string{"Accept": "text/html"})
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "no UI configured")
}

func TestSettingsChangesApplyToRBAC(t *testing.T) {
	t.Parallel()
	f := newFixture(t, apiserver.Config{Insecure: true})
	c := testClient(t, f, "dave")
	_, err := c.GetAccount(context.Background(), "admin")
	require.Equal(t, http.StatusForbidden, apiErr(t, err).StatusCode)

	f.settings.set(f.cm, map[string]string{"policy.csv": "g, dave, role:admin"}, f.secret)
	_, err = c.GetAccount(context.Background(), "admin")
	require.NoError(t, err, "patrol-rbac-cm changes apply without restart")

	// Rotating server.secretkey invalidates sessions signed with the old key.
	rotated := maps.Clone(f.secret)
	rotated[auth.KeyServerSecretKey] = strings.Repeat("r", 32)
	f.settings.set(f.cm, map[string]string{"policy.csv": "g, dave, role:admin"}, rotated)
	_, err = c.GetAccount(context.Background(), "admin")
	require.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
}

// sessionCookie returns the session cookie set on a response, or nil.
func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.SessionCookieName {
			return c
		}
	}
	return nil
}

// TestSessionCookieAttributes: login, renewal, logout and both ways of clearing a bad cookie give
// the session cookie the same attributes. Behind a TLS-terminating proxy the server sees plain
// HTTP, and only an https url tells it that browsers use TLS. TestSSOCookieAttributes in
// internal/apiserver/core covers the SSO answers.
func TestSessionCookieAttributes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		insecure   bool
		url        string
		additional string
		host       string
		tls        bool
		secure     bool
	}{
		{name: "TLS terminated by a proxy", insecure: true, url: "https://patrol.example.com", secure: true},
		{name: "url scheme in upper case", insecure: true, url: "HTTPS://patrol.example.com", secure: true},
		{name: "plain HTTP", insecure: true, url: "http://patrol.example.com"},
		{name: "plain HTTP with no url", insecure: true},
		{name: "request over TLS", insecure: true, url: "http://patrol.example.com", tls: true, secure: true},
		{name: "server with TLS", url: "http://patrol.example.com", secure: true},
		{name: "https additional url for the request host", insecure: true, url: "http://patrol.internal", additional: "- https://patrol.example.com\n", host: "patrol.example.com", secure: true},
		{name: "plain HTTP beside an https additional url", insecure: true, url: "http://patrol.internal", additional: "- https://patrol.example.com\n", host: "patrol.internal"},
		{name: "plain HTTP additional url for the request host", insecure: true, url: "http://patrol.internal", additional: "- http://patrol.example.com\n", host: "patrol.example.com"},
		{name: "https url with a plain HTTP additional url", insecure: true, url: "https://patrol.example.com", additional: "- http://patrol.internal\n", host: "patrol.internal", secure: true},
		{name: "https additional url for the host of an http url", insecure: true, url: "http://patrol.example.com", additional: "- https://patrol.example.com\n", host: "patrol.example.com", secure: true},
		{name: "https additional url after an http one for the same host", insecure: true, url: "http://patrol.internal", additional: "- http://patrol.example.com\n- https://patrol.example.com\n", host: "patrol.example.com", secure: true},
		{name: "additional url with a default port", insecure: true, url: "http://patrol.internal", additional: "- https://patrol.example.com:443\n", host: "patrol.example.com", secure: true},
		{name: "request host with a default port", insecure: true, url: "http://patrol.internal", additional: "- https://patrol.example.com\n", host: "patrol.example.com:443", secure: true},
		{name: "additional url on a port the proxy drops", insecure: true, url: "http://patrol.internal", additional: "- https://patrol.example.com:8443\n", host: "patrol.example.com", secure: true},
		{name: "additional url in upper case", insecure: true, url: "http://patrol.internal", additional: "- HTTPS://Patrol.Example.com\n", host: "patrol.example.com", secure: true},
		{name: "request host with a trailing dot", insecure: true, url: "http://patrol.internal", additional: "- https://patrol.example.com\n", host: "patrol.example.com.", secure: true},
		{name: "https additional url after one that does not parse", insecure: true, url: "http://patrol.internal", additional: "- \"https://[bad\"\n- https://patrol.example.com\n", host: "patrol.example.com", secure: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, apiserver.Config{Insecure: tc.insecure, RootPath: "patrol"})
			f.cm["url"] = tc.url
			if tc.additional != "" {
				f.cm["additionalUrls"] = tc.additional
			}
			f.settings.set(f.cm, map[string]string{}, f.secret)
			acct, err := f.settings.Current().Accounts().Get("admin")
			require.NoError(t, err)
			session, err := f.tokens.IssueLoginSession(context.Background(), acct, 0)
			require.NoError(t, err)
			expiring, err := f.tokens.IssueLoginSession(context.Background(), acct, time.Minute)
			require.NoError(t, err)

			steps := []struct {
				name, method, path, body, cookie string
				status                           int
				clears                           bool
			}{
				{name: "login", method: http.MethodPost, path: apiclient.PathSession + "?cookie=true", body: `{"username":"admin","password":"Password1!"}`, status: http.StatusOK},
				{name: "renewal", method: http.MethodGet, path: apiclient.PathSessionUserInfo, cookie: expiring.Token, status: http.StatusOK},
				{name: "cookie that is not a token", method: http.MethodGet, path: apiclient.PathSessionUserInfo, cookie: "not-a-token", status: http.StatusOK, clears: true},
				{name: "token that does not verify", method: http.MethodGet, path: apiclient.PathSessionUserInfo, cookie: "aaa.bbb.ccc", status: http.StatusOK, clears: true},
				{name: "logout", method: http.MethodDelete, path: apiclient.PathSession, cookie: session.Token, status: http.StatusOK, clears: true},
			}
			for _, step := range steps {
				req := httptest.NewRequest(step.method, "/patrol"+step.path, strings.NewReader(step.body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(rest.CSRFHeader, rest.CSRFHeaderValue)
				if step.cookie != "" {
					req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: step.cookie})
				}
				if tc.host != "" {
					req.Host = tc.host
				}
				if tc.tls {
					req.TLS = &tls.ConnectionState{}
				}
				rec := httptest.NewRecorder()
				f.srv.Handler().ServeHTTP(rec, req)

				require.Equal(t, step.status, rec.Code, step.name)
				got := sessionCookie(rec)
				require.NotNil(t, got, step.name)
				assert.Equal(t, tc.secure, got.Secure, step.name)
				assert.Equal(t, "/patrol", got.Path, step.name)
				assert.Equal(t, step.clears, got.Value == "", step.name)
			}
		})
	}
}

// TestSessionCookieWithoutSettings: before the first settings snapshot a bad cookie is still
// cleared with the attributes the server knows from its own configuration.
func TestSessionCookieWithoutSettings(t *testing.T) {
	t.Parallel()
	f := newFixture(t, apiserver.Config{RootPath: "patrol"})
	f.settings.cur.Store(nil)
	req := httptest.NewRequest(http.MethodGet, "/patrol"+apiclient.PathSessionUserInfo, nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "not-a-token"})
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)

	got := sessionCookie(rec)
	require.NotNil(t, got)
	assert.Empty(t, got.Value)
	assert.True(t, got.Secure)
	assert.Equal(t, "/patrol", got.Path)
}

func TestNewValidation(t *testing.T) {
	t.Parallel()
	_, err := apiserver.New(apiserver.Config{}, apiserver.Deps{})
	require.Error(t, err)
	fs := &fakeSettings{}
	fs.set(map[string]string{}, map[string]string{}, map[string]string{})
	_, err = apiserver.New(apiserver.Config{}, apiserver.Deps{Settings: fs})
	require.ErrorContains(t, err, "token service")
	srv, err := apiserver.New(apiserver.Config{DisableAuth: true, Insecure: true}, apiserver.Deps{Settings: fs})
	require.NoError(t, err)
	resp := serve(t, srv.Handler(), http.MethodGet, "/api/v1/settings", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	dup := apiserver.ServiceFunc(func(t *rest.Table) {
		rest.Register(t, rest.Route{Method: http.MethodGet, Path: "/api/v1/settings", OperationID: "dup", Access: rest.Public()},
			func(context.Context, *apiclient.Empty) (*apiclient.Empty, error) { return nil, nil })
	})
	_, err = apiserver.New(apiserver.Config{DisableAuth: true}, apiserver.Deps{Settings: fs, Services: []apiserver.Service{dup}})
	require.ErrorContains(t, err, "registered twice")
}

func TestServeTLSRedirectMetricsAndShutdown(t *testing.T) {
	t.Parallel()
	streamStarted := make(chan struct{})
	streamEnded := make(chan error, 1)
	svc := apiserver.ServiceFunc(func(t *rest.Table) {
		rest.RegisterStream(t, rest.Route{Method: http.MethodGet, Path: "/api/v1/stream/test", OperationID: "streamTest", Access: rest.Public()},
			rest.StreamOptions[apiclient.Empty]{Heartbeat: time.Hour},
			func(ctx context.Context, _ *apiclient.Empty, _ *rest.Sink[apiclient.Empty]) error {
				close(streamStarted)
				<-ctx.Done()
				streamEnded <- ctx.Err()
				return ctx.Err()
			})
	})
	f := newFixture(t, apiserver.Config{RootPath: "/patrol", ShutdownTimeout: 5 * time.Second, TLS: apiserver.TLSOptions{MinVersion: "1.2", Hosts: []string{"127.0.0.1"}}},
		func(d *apiserver.Deps) { d.Services = []apiserver.Service{svc} })
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ml, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.srv.Serve(ctx, l, ml) }()
	addr := l.Addr().String()

	tlsClient := &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // self-signed test certificate
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := tlsClient.Get("https://" + addr + "/patrol/api/version")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, readAll(t, resp))
	require.NotNil(t, resp.TLS)
	assert.Equal(t, "patrol-server", resp.TLS.PeerCertificates[0].Subject.CommonName, "self-signed certificate when none is configured")
	assert.Equal(t, "max-age=31536000", resp.Header.Get("Strict-Transport-Security"))

	// Plain HTTP on the TLS port is redirected once, root path included.
	resp, err = tlsClient.Get("http://" + addr + "/patrol/applications?x=1")
	require.NoError(t, err)
	_ = readAll(t, resp)
	assert.Equal(t, http.StatusTemporaryRedirect, resp.StatusCode)
	assert.Equal(t, "https://"+addr+"/patrol/applications?x=1", resp.Header.Get("Location"))

	// TLS 1.1 is refused.
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: tls.VersionTLS11}) //nolint:gosec // negative test
	if err == nil {
		_ = conn.Close()
		t.Fatal("TLS 1.1 handshake succeeded")
	}

	// Metrics on the separate port.
	resp, err = http.Get("http://" + ml.Addr().String() + "/metrics")
	require.NoError(t, err)
	assert.Contains(t, readAll(t, resp), `patrol_server_http_requests_total{code="200",method="GET",operation="getVersionAlias"}`)

	// An open stream does not block shutdown; /healthz turns 503 meanwhile.
	sreq, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/patrol/api/v1/stream/test", nil)
	sresp, err := tlsClient.Do(sreq)
	require.NoError(t, err)
	defer func() { _ = sresp.Body.Close() }()
	<-streamStarted
	cancel()
	select {
	case err := <-streamEnded:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("stream not ended by shutdown")
	}
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("server did not shut down")
	}
	_, _ = io.Copy(io.Discard, bufio.NewReader(sresp.Body))
}

func TestHealthzReadiness(t *testing.T) {
	t.Parallel()
	ready := atomic.Bool{}
	f := newFixture(t, apiserver.Config{Insecure: true}, func(d *apiserver.Deps) {
		d.Ready = func(context.Context) error {
			if !ready.Load() {
				return apierrors.NewServiceUnavailable("syncing")
			}
			return nil
		}
	})
	assert.Equal(t, http.StatusServiceUnavailable, serve(t, f.srv.Handler(), http.MethodGet, "/healthz", nil).StatusCode)
	ready.Store(true)
	resp := serve(t, f.srv.Handler(), http.MethodGet, "/healthz", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", readAll(t, resp))
}

func TestProjectTokenRegistry(t *testing.T) {
	t.Parallel()
	proj := &v1alpha1.AppProject{
		ObjectMeta: metav1.ObjectMeta{Name: "proj"},
		Spec:       v1alpha1.AppProjectSpec{Roles: []v1alpha1.ProjectRole{{Name: "ci"}}},
		Status: v1alpha1.AppProjectStatus{JWTTokensByRole: map[string]v1alpha1.JWTTokens{
			"ci":   {Items: []v1alpha1.JWTToken{{ID: "jti-1", IssuedAt: 100}, {IssuedAt: 200}}},
			"gone": {Items: []v1alpha1.JWTToken{{ID: "jti-9", IssuedAt: 900}}},
		}},
	}
	get := func(_ context.Context, name string) (*v1alpha1.AppProject, error) {
		if name == "proj" {
			return proj, nil
		}
		if name == "broken" {
			return nil, apierrors.NewServiceUnavailable("etcd")
		}
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "cd.patrol.example", Resource: "appprojects"}, name)
	}
	reg := apiserver.ProjectTokenRegistry(get)
	ctx := context.Background()
	tests := []struct {
		name    string
		sub     string
		jti     string
		iat     int64
		want    bool
		wantErr bool
	}{
		{"by id", "proj:proj:ci", "jti-1", 100, true, false},
		{"unknown id", "proj:proj:ci", "jti-2", 100, false, false},
		{"legacy by iat", "proj:proj:ci", "", 200, true, false},
		{"role removed", "proj:proj:gone", "jti-9", 900, false, false},
		{"project missing", "proj:other:ci", "jti-1", 100, false, false},
		{"not a project subject", "admin", "jti-1", 100, false, false},
		{"api failure", "proj:broken:ci", "jti-1", 100, false, true},
	}
	for _, tc := range tests {
		got, err := reg.IsTokenKnown(ctx, tc.sub, tc.jti, tc.iat)
		if tc.wantErr {
			require.Error(t, err, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		assert.Equal(t, tc.want, got, tc.name)
	}
}

func TestTLSHelpers(t *testing.T) {
	t.Parallel()
	v, err := apiserver.ParseTLSVersion("1.3")
	require.NoError(t, err)
	assert.Equal(t, uint16(tls.VersionTLS13), v)
	_, err = apiserver.ParseTLSVersion("2.0")
	require.Error(t, err)
	ids, err := apiserver.ParseCipherSuites([]string{"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"})
	require.NoError(t, err)
	assert.Equal(t, []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}, ids)
	_, err = apiserver.ParseCipherSuites([]string{"TLS_RSA_WITH_RC4_128_SHA"})
	require.ErrorContains(t, err, "insecure")
	_, err = apiserver.ParseCipherSuites([]string{"NOPE"})
	require.ErrorContains(t, err, "unknown")
	cert, err := apiserver.GenerateSelfSigned([]string{"localhost", "127.0.0.1"}, time.Hour)
	require.NoError(t, err)
	assert.Equal(t, []string{"localhost"}, cert.Leaf.DNSNames)
	assert.Len(t, cert.Leaf.IPAddresses, 1)
}

func testClient(t *testing.T, f *fixture, user string) *apiclient.Client {
	t.Helper()
	srv := httptest.NewServer(f.srv.Handler())
	t.Cleanup(srv.Close)
	c, err := apiclient.NewClient(&apiclient.ClientOptions{ServerAddr: srv.URL})
	require.NoError(t, err)
	_, err = c.Login(context.Background(), user, "Password1!")
	require.NoError(t, err)
	return c
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return string(b)
}

func apiErr(t *testing.T, err error) *apiclient.APIError {
	t.Helper()
	require.Error(t, err)
	var ae *apiclient.APIError
	require.ErrorAs(t, err, &ae)
	return ae
}

// TestServeTLSShutdownIdle is a regression test for a shutdown race: the TLS server and
// the HTTPS-redirect server share one listener, and closing it twice made a graceful
// shutdown with no in-flight requests report "use of closed network connection".
func TestServeTLSShutdownIdle(t *testing.T) {
	t.Parallel()
	// One Server is served repeatedly (each Serve builds fresh http.Servers and listeners);
	// building a Server per iteration is slow under -race.
	f := newFixture(t, apiserver.Config{ShutdownTimeout: 5 * time.Second, TLS: apiserver.TLSOptions{Hosts: []string{"127.0.0.1"}}})
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // self-signed test certificate
		DisableKeepAlives: true,
	}}
	for i := range 40 {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- f.srv.Serve(ctx, l, nil) }()
		resp, err := client.Get("https://" + l.Addr().String() + "/api/version")
		require.NoError(t, err, "iteration %d", i)
		require.Equal(t, http.StatusOK, resp.StatusCode, readAll(t, resp))
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err, "iteration %d: graceful shutdown without in-flight requests", i)
		case <-time.After(10 * time.Second):
			t.Fatalf("iteration %d: server did not shut down", i)
		}
	}
}
