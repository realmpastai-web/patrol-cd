package core_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	authoidc "github.com/juliosaraiva/patrol-cd/internal/auth/oidc"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func ssoEnv(t *testing.T, idp *fakeIdP, extra string) *testEnv {
	t.Helper()
	cm := defaultCM()
	cm["oidc.config"] = idp.oidcConfig(extra)
	return newEnv(t, envOptions{cm: cm, sso: &core.SSOConfig{}})
}

// logBuffer holds what the server logs from its request goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
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

func cookieNamed(cs []*http.Cookie, name string) *http.Cookie {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func TestSSOLoginFlow(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	e := ssoEnv(t, idp, "enablePKCEAuthentication: true\n")
	b := e.browser()

	returnURL := e.url + "/applications?proj=default"
	resp, err := b.Get(e.url + apiclient.PathAuthLogin + "?return_url=" + url.QueryEscape(returnURL))
	require.NoError(t, err)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	authURL, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, idp.srv.URL+"/auth", authURL.Scheme+"://"+authURL.Host+authURL.Path)
	q := authURL.Query()
	assert.Equal(t, e.url+apiclient.PathAuthCallback, q.Get("redirect_uri"))
	assert.Equal(t, "S256", q.Get("code_challenge_method"))
	assert.NotEmpty(t, q.Get("nonce"))
	assert.NotEmpty(t, q.Get("state"))
	state := cookieNamed(resp.Cookies(), authoidc.StateCookieName)
	require.NotNil(t, state)
	assert.True(t, state.HttpOnly)

	// The browser visits the IdP, which redirects back to the callback.
	resp, err = b.Get(authURL.String())
	require.NoError(t, err)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusFound, resp.StatusCode)
	callback := resp.Header.Get("Location")
	require.True(t, strings.HasPrefix(callback, e.url+apiclient.PathAuthCallback), callback)

	resp, err = b.Get(callback)
	require.NoError(t, err)
	body := readBody(t, resp)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
	assert.Equal(t, returnURL, resp.Header.Get("Location"))
	session := cookieNamed(resp.Cookies(), auth.SessionCookieName)
	require.NotNil(t, session)
	cleared := cookieNamed(resp.Cookies(), authoidc.StateCookieName)
	require.NotNil(t, cleared)
	assert.Negative(t, cleared.MaxAge, "the state cookie is cleared")

	p, err := e.tokens.Verify(context.Background(), session.Value)
	require.NoError(t, err)
	assert.Equal(t, auth.KindSSO, p.Kind)
	assert.Equal(t, "user-123", p.Subject)

	resp, err = b.Get(e.url + apiclient.PathSessionUserInfo)
	require.NoError(t, err)
	assert.JSONEq(t, `{"loggedIn":true,"username":"jane@example.com","iss":"patrol","groups":["devs","admins"]}`, readBody(t, resp))

	// Group claims drive RBAC: "admins" is bound to role:admin.
	resp, err = b.Get(e.url + apiclient.PathAccounts)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))

	audits := e.audit.byOperation("ssoCallback")
	require.Len(t, audits, 1)
	assert.Equal(t, "user-123", audits[0].Actor)
	assert.Equal(t, rest.AuditSucceeded, audits[0].Result)
	assert.Equal(t, float64(1), e.loginCount("sso", "success"))

	// Replaying the callback fails: the state cookie is gone.
	resp, err = b.Get(callback)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, readBody(t, resp))

	// Logout of an SSO session returns the IdP logout URL when configured.
	e.holder.setCM(func(cm map[string]string) {
		cm["oidc.config"] = idp.oidcConfig("logoutURL: " + idp.srv.URL + "/logout?redirect={{logoutRedirectURL}}\n")
	})
	c := e.clientWithToken(session.Value)
	out, err := c.Logout(context.Background())
	require.NoError(t, err)
	assert.Equal(t, idp.srv.URL+"/logout?redirect="+url.QueryEscape(e.url), out.LogoutURL)
}

// TestSSOCookieAttributes: the state cookie, its clearing and the session cookie follow
// Config.CookieOptions. Behind a TLS-terminating proxy the server sees plain HTTP and redirects to
// a host the test does not serve, so the test brings the callback and the state cookie back itself.
func TestSSOCookieAttributes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		url        string
		additional string
		host       string
		secure     bool
	}{
		{name: "TLS terminated by a proxy", url: "https://patrol.example.com", secure: true},
		{name: "plain HTTP"},
		{name: "https additional url for the request host", url: "http://patrol.internal", additional: "- https://patrol.example.com\n", host: "patrol.example.com", secure: true},
		{name: "plain HTTP beside an https additional url", url: "http://patrol.internal", additional: "- https://patrol.example.com\n", host: "patrol.internal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			idp := newFakeIdP(t)
			cm := defaultCM()
			cm["oidc.config"] = idp.oidcConfig("")
			if tc.url != "" {
				cm["url"] = tc.url
			}
			if tc.additional != "" {
				cm["additionalUrls"] = tc.additional
			}
			e := newEnv(t, envOptions{cm: cm, sso: &core.SSOConfig{}, baseHRef: "/patrol/"})
			b := e.browser()
			b.Jar = nil

			req, _ := http.NewRequest(http.MethodGet, e.url+apiclient.PathAuthLogin, nil)
			req.Host = tc.host
			resp, err := b.Do(req)
			require.NoError(t, err)
			_ = readBody(t, resp)
			require.Equal(t, http.StatusSeeOther, resp.StatusCode)
			state := cookieNamed(resp.Cookies(), authoidc.StateCookieName)
			require.NotNil(t, state)
			assert.Equal(t, tc.secure, state.Secure)
			assert.Equal(t, "/patrol", state.Path)

			resp, err = b.Get(resp.Header.Get("Location"))
			require.NoError(t, err)
			_ = readBody(t, resp)
			require.Equal(t, http.StatusFound, resp.StatusCode)
			callback, err := url.Parse(resp.Header.Get("Location"))
			require.NoError(t, err)

			req, _ = http.NewRequest(http.MethodGet, e.url+callback.RequestURI(), nil)
			req.Host = tc.host
			req.AddCookie(&http.Cookie{Name: state.Name, Value: state.Value})
			resp, err = b.Do(req)
			require.NoError(t, err)
			body := readBody(t, resp)
			require.Equal(t, http.StatusSeeOther, resp.StatusCode, body)
			for _, name := range []string{authoidc.StateCookieName, auth.SessionCookieName} {
				c := cookieNamed(resp.Cookies(), name)
				require.NotNil(t, c, name)
				assert.Equal(t, tc.secure, c.Secure, name)
				assert.Equal(t, "/patrol", c.Path, name)
			}
		})
	}
}

func TestSSOLoginErrors(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	e := ssoEnv(t, idp, "")

	b := e.browser()
	resp, err := b.Get(e.url + apiclient.PathAuthLogin + "?return_url=" + url.QueryEscape("https://evil.example.com/steal"))
	require.NoError(t, err)
	body := readBody(t, resp)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, body, "Invalid redirect URL")
	assert.Empty(t, resp.Header.Get("Location"))

	// A callback without the state cookie (login CSRF) is rejected.
	resp, err = b.Get(e.url + apiclient.PathAuthCallback + "?code=abc&state=forged")
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, readBody(t, resp))

	// A state that does not match the cookie is rejected.
	resp, err = b.Get(e.url + apiclient.PathAuthLogin)
	require.NoError(t, err)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, err = b.Get(e.url + apiclient.PathAuthCallback + "?code=abc&state=forged")
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, readBody(t, resp))

	// IdP errors are reported without echoing the description.
	resp, err = b.Get(e.url + apiclient.PathAuthLogin)
	require.NoError(t, err)
	_ = readBody(t, resp)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	resp, err = b.Get(e.url + apiclient.PathAuthCallback + "?error=access_denied&error_description=%3Cscript%3E&state=" + loc.Query().Get("state"))
	require.NoError(t, err)
	body = readBody(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	assert.Contains(t, body, "access_denied")
	assert.NotContains(t, body, "<script>")
	assert.GreaterOrEqual(t, e.loginCount("sso", "failure"), float64(1))

	// Without SSO configured the routes answer 404.
	plain := newEnv(t, envOptions{sso: &core.SSOConfig{}})
	resp, err = plain.browser().Get(plain.url + apiclient.PathAuthLogin)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, readBody(t, resp))
	_, err = plain.client("").ExchangeOIDCToken(context.Background(), &apiclient.OIDCExchangeRequest{IDToken: "a.b.c"})
	assert.Equal(t, http.StatusNotFound, apiErr(t, err).StatusCode)
}

// TestSSOKeySetRefused pins the answer to an ID token that fails verification, here because
// go-jose refuses the key set: a 401 without the reason, which goes to the log, where v3.5.5
// answers 500.
func TestSSOKeySetRefused(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	idp.mu.Lock()
	idp.extraJWK = map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "unused", "e": "AQAB",
		"n": base64.RawURLEncoding.EncodeToString(new(big.Int).Lsh(big.NewInt(1), 4096).Bytes()), // 4097 bits
	}
	idp.mu.Unlock()
	var log logBuffer
	cm := defaultCM()
	cm["oidc.config"] = idp.oidcConfig("")
	e := newEnv(t, envOptions{cm: cm, sso: &core.SSOConfig{}, logger: slog.New(slog.NewJSONHandler(&log, nil))})
	b := e.browser()

	resp, err := b.Get(e.url + apiclient.PathAuthLogin)
	require.NoError(t, err)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	resp, err = b.Get(resp.Header.Get("Location"))
	require.NoError(t, err)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusFound, resp.StatusCode)

	resp, err = b.Get(resp.Header.Get("Location"))
	require.NoError(t, err)
	body := readBody(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, body)
	assert.Contains(t, body, "ID token verification failed")
	assert.NotContains(t, body, "go-jose")
	assert.Nil(t, cookieNamed(resp.Cookies(), auth.SessionCookieName))
	assert.Equal(t, float64(1), e.loginCount("sso", "failure"))
	assert.Regexp(t, `"msg":"SSO login failed".*go-jose/go-jose: invalid RSA key, modulus too large`, log.String())
}

func TestOIDCExchange(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	e := ssoEnv(t, idp, "")
	ctx := context.Background()
	c := e.client("")

	_, err := c.ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{IDToken: idp.idToken(map[string]any{"aud": idp.cliID})})
	require.NoError(t, err)
	ui, err := c.UserInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, &apiclient.UserInfo{LoggedIn: true, Username: "jane@example.com", Iss: auth.Issuer, Groups: []string{"devs", "admins"}}, ui)
	_, err = c.ListAccounts(ctx)
	require.NoError(t, err, "the exchanged session carries the groups for RBAC")
	audits := e.audit.byOperation("exchangeOIDCToken")
	require.Len(t, audits, 1)
	assert.Equal(t, "user-123", audits[0].Actor)

	tests := []struct {
		name     string
		override map[string]any
		status   int
	}{
		{"foreign audience", map[string]any{"aud": "someone-else"}, http.StatusUnauthorized},
		{"too old", map[string]any{"aud": idp.cliID, "iat": time.Now().Add(-10 * time.Minute).Unix()}, http.StatusUnauthorized},
		{"expired", map[string]any{"aud": idp.cliID, "exp": time.Now().Add(-time.Minute).Unix()}, http.StatusUnauthorized},
		{"wrong issuer", map[string]any{"aud": idp.cliID, "iss": "https://evil.example.com"}, http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.client("").ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{IDToken: idp.idToken(tc.override)})
			assert.Equal(t, tc.status, apiErr(t, err).StatusCode)
		})
	}
	_, err = e.client("").ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{})
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode)
	_, err = e.client("").ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{IDToken: idp.idToken(map[string]any{"aud": idp.cliID}) + "x"})
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode, "bad signature")
}

func TestOIDCExchangeUserInfoGroups(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	e := ssoEnv(t, idp, "enableUserInfoGroups: true\nuserInfoPath: /userinfo\n")
	ctx := context.Background()
	idt := idp.idToken(map[string]any{"aud": idp.cliID})
	_, err := e.client("").ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{IDToken: idt})
	assert.Equal(t, http.StatusBadRequest, apiErr(t, err).StatusCode, "the access token is required")

	c := e.client("")
	_, err = c.ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{IDToken: idt, AccessToken: idp.accessToken()})
	require.NoError(t, err)
	ui, err := c.UserInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"from-userinfo"}, ui.Groups)

	_, err = e.client("").ExchangeOIDCToken(ctx, &apiclient.OIDCExchangeRequest{IDToken: idt, AccessToken: "unknown"})
	require.Error(t, err)
}

// fakeDex records proxied requests.
type fakeDex struct {
	mu   sync.Mutex
	reqs []*http.Request
	srv  *httptest.Server
	ext  string
}

func newFakeDex(t *testing.T) *fakeDex {
	t.Helper()
	d := &fakeDex{}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.reqs = append(d.reqs, r.Clone(context.Background()))
		ext := d.ext
		d.mu.Unlock()
		switch r.URL.Path {
		case "/api/dex/.well-known/openid-configuration":
			writeJSON(w, map[string]any{
				"issuer": ext + "/api/dex", "authorization_endpoint": ext + "/api/dex/auth", "token_endpoint": ext + "/api/dex/token",
				"jwks_uri": ext + "/api/dex/keys", "response_types_supported": []string{"code"},
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		case "/api/dex/fail":
			http.Error(w, "internal dex failure", http.StatusInternalServerError)
		default:
			_, _ = io.WriteString(w, "dex:"+r.URL.Path)
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDex) last() *http.Request {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reqs[len(d.reqs)-1]
}

func TestDexProxy(t *testing.T) {
	t.Parallel()
	dex := newFakeDex(t)
	cm := defaultCM()
	cm["dex.config"] = "connectors:\n- type: github\n  id: github\n  name: GitHub\n  config:\n    clientID: x\n    clientSecret: y\n"
	e := newEnv(t, envOptions{cm: cm, sso: &core.SSOConfig{DexServerURL: dex.srv.URL}})
	dex.mu.Lock()
	dex.ext = e.url
	dex.mu.Unlock()
	b := e.browser()

	req, _ := http.NewRequest(http.MethodGet, e.url+"/api/dex/.well-known/openid-configuration", nil)
	req.Header.Set("Authorization", "Bearer "+e.client("alice").Token())
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "secret-session"})
	req.AddCookie(&http.Cookie{Name: "other", Value: "kept"})
	resp, err := b.Do(req)
	require.NoError(t, err)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Contains(t, body, `"issuer":"`+e.url+`/api/dex"`)
	got := dex.last()
	assert.Equal(t, "/api/dex/.well-known/openid-configuration", got.URL.Path)
	assert.Empty(t, got.Header.Get("Authorization"), "Patrol credentials are not forwarded to Dex")
	_, err = got.Cookie(auth.SessionCookieName)
	assert.Error(t, err)
	other, err := got.Cookie("other")
	require.NoError(t, err)
	assert.Equal(t, "kept", other.Value)
	assert.Equal(t, strings.TrimPrefix(e.url, "http://"), got.Host)

	// Dex's own credentials in Authorization are forwarded (upstream forwards every
	// header): client_secret_basic on /token, Dex access tokens on /userinfo. Only
	// Patrol-issued bearer tokens are removed.
	dexJWT, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": e.url + "/api/dex", "sub": "u-1"}).SignedString([]byte("dex-test-key"))
	require.NoError(t, err)
	patrolTok := e.client("alice").Token()
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("other-app:client-secret"))
	authCases := []struct {
		name   string
		method string
		path   string
		values []string
		want   []string
	}{
		{"client_secret_basic on /token", http.MethodPost, "/api/dex/token", []string{basic}, []string{basic}},
		{"opaque Dex access token on /userinfo", http.MethodGet, "/api/dex/userinfo", []string{"Bearer dex-opaque-access-token"}, []string{"Bearer dex-opaque-access-token"}},
		{"Dex-issued JWT on /userinfo", http.MethodGet, "/api/dex/userinfo", []string{"Bearer " + dexJWT}, []string{"Bearer " + dexJWT}},
		{"Patrol bearer, lower-case scheme", http.MethodGet, "/api/dex/userinfo", []string{"bearer " + patrolTok}, nil},
		{"Patrol bearer next to Basic", http.MethodPost, "/api/dex/token", []string{"Bearer " + patrolTok, basic}, []string{basic}},
	}
	for _, tc := range authCases {
		var body io.Reader
		if tc.method == http.MethodPost {
			body = strings.NewReader("grant_type=authorization_code&code=abc")
		}
		req, _ := http.NewRequest(tc.method, e.url+tc.path, body)
		if body != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		for _, v := range tc.values {
			req.Header.Add("Authorization", v)
		}
		resp, err := b.Do(req)
		require.NoError(t, err, tc.name)
		rb := readBody(t, resp)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", tc.name, rb)
		got := dex.last()
		assert.Equal(t, tc.path, got.URL.Path, tc.name)
		assert.Equal(t, tc.want, got.Header.Values("Authorization"), tc.name)
	}

	// Dex form posts need no CSRF header even with a session cookie.
	req, _ = http.NewRequest(http.MethodPost, e.url+"/api/dex/auth/local/login", strings.NewReader("login=a&password=b"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: e.client("alice").Token()})
	resp, err = b.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))

	resp, err = b.Get(e.url + "/api/dex/fail")
	require.NoError(t, err)
	body = readBody(t, resp)
	assert.Equal(t, http.StatusSeeOther, resp.StatusCode)
	assert.Equal(t, "/login?has_sso_error=true", resp.Header.Get("Location"))
	assert.NotContains(t, body, "internal dex failure")

	// The login flow discovers the Dex issuer (the external URL) through the internal
	// Dex address.
	resp, err = b.Get(e.url + apiclient.PathAuthLogin)
	require.NoError(t, err)
	_ = readBody(t, resp)
	require.Equal(t, http.StatusSeeOther, resp.StatusCode)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, e.url+"/api/dex/auth", loc.Scheme+"://"+loc.Host+loc.Path)
	assert.Equal(t, authoidc.DexClientID, loc.Query().Get("client_id"))
	assert.Contains(t, loc.Query().Get("scope"), authoidc.DexFederatedScope)

	// Without dex.config the proxy answers 404.
	plain := newEnv(t, envOptions{sso: &core.SSOConfig{DexServerURL: dex.srv.URL}})
	resp, err = plain.browser().Get(plain.url + "/api/dex/.well-known/openid-configuration")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, readBody(t, resp))

	_, err = core.NewSSO(core.SSOConfig{DexServerURL: "ftp://dex"})
	require.Error(t, err)
}

func TestDexTransport(t *testing.T) {
	t.Parallel()
	tr, err := core.DexTransport(false, nil)
	require.NoError(t, err)
	assert.True(t, tr.(*http.Transport).TLSClientConfig.InsecureSkipVerify)
	_, err = core.DexTransport(true, nil)
	require.Error(t, err)

	dex := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	t.Cleanup(dex.Close)
	pool := x509.NewCertPool()
	pool.AddCert(dex.Certificate())
	strict, err := core.DexTransport(true, pool)
	require.NoError(t, err)
	resp, err := (&http.Client{Transport: strict}).Get(dex.URL)
	require.NoError(t, err)
	_ = readBody(t, resp)
	other, err := core.DexTransport(true, x509.NewCertPool())
	require.NoError(t, err)
	_, err = (&http.Client{Transport: other}).Get(dex.URL)
	require.Error(t, err, "strict mode rejects an unknown certificate")
}

func TestIdPTokenAsBearer(t *testing.T) {
	t.Parallel()
	idp := newFakeIdP(t)
	e := ssoEnv(t, idp, "")
	ctx := context.Background()
	c := e.clientWithToken(idp.idToken(map[string]any{"aud": idp.cliID}))
	ui, err := c.UserInfo(ctx)
	require.NoError(t, err)
	assert.True(t, ui.LoggedIn)
	assert.Equal(t, idp.srv.URL, ui.Iss, "IdP tokens report the IdP issuer")
	assert.Equal(t, []string{"devs", "admins"}, ui.Groups)
	_, err = c.ListAccounts(ctx)
	require.NoError(t, err, "IdP groups drive RBAC")

	bad := e.clientWithToken(idp.idToken(map[string]any{"aud": "someone-else"}))
	_, err = bad.ListAccounts(ctx)
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)

	// Without SSO, external tokens are invalid.
	plain := newEnv(t, envOptions{sso: &core.SSOConfig{}})
	_, err = plain.clientWithToken(idp.idToken(nil)).ListAccounts(ctx)
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
}
