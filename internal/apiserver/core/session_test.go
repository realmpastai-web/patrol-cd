package core_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func TestLocalLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	c := e.client("")

	resp, err := c.Login(ctx, "admin", testPassword)
	require.NoError(t, err)
	require.NotEmpty(t, resp.Token)
	p, err := e.tokens.Verify(ctx, resp.Token)
	require.NoError(t, err)
	assert.Equal(t, auth.KindLogin, p.Kind)
	assert.Equal(t, "admin", p.Subject)
	assert.WithinDuration(t, e.clock.Now().Add(time.Hour), p.ExpiresAt, time.Second, "users.session.duration")

	ui, err := c.UserInfo(ctx)
	require.NoError(t, err)
	assert.Equal(t, &apiclient.UserInfo{LoggedIn: true, Username: "admin", Iss: auth.Issuer}, ui)

	audits := e.audit.byOperation("createSession")
	require.Len(t, audits, 1)
	assert.Equal(t, "admin", audits[0].Actor)
	assert.Equal(t, rest.AuditSucceeded, audits[0].Result)
	assert.Equal(t, float64(1), e.loginCount("local", "success"))

	tests := []struct {
		name, user, password string
		status               int
		detail               string
	}{
		{"wrong password", "admin", "nope-nope", 401, "Invalid username or password"},
		{"unknown user", "mallory", "nope-nope", 401, "Invalid username or password"},
		{"no password set", "ci", testPassword, 401, "Invalid username or password"},
		{"no credentials", "", "", 401, "no credentials supplied"},
		{"blank password", "admin", "", 401, "no credentials supplied"},
		{"disabled", "bob", testPassword, 401, "Account bob is disabled"},
		{"username too long", strings.Repeat("u", 40), "x", 400, "Username is too long"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.client("").Login(ctx, tc.user, tc.password)
			ae := apiErr(t, err)
			assert.Equal(t, tc.status, ae.StatusCode)
			assert.Contains(t, ae.Problem.Detail, tc.detail)
		})
	}
	failed := e.audit.byOperation("createSession")
	require.Greater(t, len(failed), 1)
	for _, a := range failed[1:] {
		assert.NotEqual(t, rest.AuditSucceeded, a.Result)
		assert.Empty(t, a.Actor, "failed logins never record the attempted username as actor")
	}
	assert.GreaterOrEqual(t, e.loginCount("local", "failure"), float64(5))
}

func TestLoginCookie(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	b := e.browser()
	resp, err := b.Post(e.url+apiclient.PathSession+"?cookie=true", "application/json",
		strings.NewReader(`{"username":"admin","password":"`+testPassword+`"}`))
	require.NoError(t, err)
	body := readBody(t, resp)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	var sc *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName {
			sc = c
		}
	}
	require.NotNil(t, sc)
	assert.True(t, sc.HttpOnly)
	assert.Equal(t, http.SameSiteStrictMode, sc.SameSite)
	assert.Equal(t, "/", sc.Path)
	assert.Contains(t, body, sc.Value)

	// The cookie authenticates reads...
	resp, err = b.Get(e.url + apiclient.PathSessionUserInfo)
	require.NoError(t, err)
	assert.Contains(t, readBody(t, resp), `"loggedIn":true`)

	// ...but cookie-authenticated mutations need the CSRF header.
	req, _ := http.NewRequest(http.MethodDelete, e.url+apiclient.PathSession, nil)
	resp, err = b.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, readBody(t, resp))

	req, _ = http.NewRequest(http.MethodDelete, e.url+apiclient.PathSession, nil)
	req.Header.Set(rest.CSRFHeader, rest.CSRFHeaderValue)
	resp, err = b.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))
	var cleared bool
	for _, c := range resp.Cookies() {
		if c.Name == auth.SessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	assert.True(t, cleared, "logout clears the session cookie")

	// The revoked session no longer verifies even if replayed.
	_, err = e.tokens.Verify(context.Background(), sc.Value)
	require.ErrorIs(t, err, auth.ErrTokenRevoked)
}

func TestLoginLockout(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	for range 3 {
		_, err := e.client("").Login(ctx, "dave", "wrong-password")
		require.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)
	}
	_, err := e.client("").Login(ctx, "dave", testPassword)
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusUnauthorized, ae.StatusCode, "locked out: the correct password is rejected")
	assert.Equal(t, "Invalid username or password", ae.Problem.Detail, "lockout is indistinguishable from a bad password")

	// Other accounts are unaffected.
	_, err = e.client("").Login(ctx, "alice", testPassword)
	require.NoError(t, err)

	// The lockout ends after the failure window.
	e.clock.Advance(auth.DefaultLoginFailureWindow + time.Second)
	_, err = e.client("").Login(ctx, "dave", testPassword)
	require.NoError(t, err)
}

func TestLogoutRevokesSession(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	c := e.client("alice")
	tok := c.Token()
	_, err := c.ListAccounts(ctx)
	require.NoError(t, err)

	out, err := c.Logout(ctx)
	require.NoError(t, err)
	assert.Empty(t, out.LogoutURL, "no logout URL for local sessions")
	assert.Empty(t, c.Token())

	replay := e.clientWithToken(tok)
	_, err = replay.ListAccounts(ctx)
	ae := apiErr(t, err)
	assert.Equal(t, http.StatusUnauthorized, ae.StatusCode)
	assert.Contains(t, ae.Problem.Detail, "token revoked")

	ui, err := replay.UserInfo(ctx)
	require.NoError(t, err, "userinfo is public")
	assert.False(t, ui.LoggedIn)

	// Logout needs a session.
	_, err = e.client("").Logout(ctx)
	assert.Equal(t, http.StatusUnauthorized, apiErr(t, err).StatusCode)

	// API tokens are not revoked by logout (delete their metadata instead).
	ci := e.client("")
	admin := e.client("admin")
	tr, err := admin.CreateAccountToken(ctx, &apiclient.CreateAccountTokenRequest{Name: "ci", ExpiresIn: 3600})
	require.NoError(t, err)
	ci.SetToken(tr.Token)
	_, err = ci.Logout(ctx)
	require.NoError(t, err)
	ci.SetToken(tr.Token)
	_, err = ci.UserInfo(ctx)
	require.NoError(t, err)
	ui, err = ci.UserInfo(ctx)
	require.NoError(t, err)
	assert.True(t, ui.LoggedIn)
}

func TestUserInfoAnonymousAndGroups(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ui, err := e.client("").UserInfo(context.Background())
	require.NoError(t, err)
	assert.Equal(t, &apiclient.UserInfo{LoggedIn: false}, ui)

	tok, err := e.tokens.IssueSSOSession(context.Background(), auth.SSOIdentity{Subject: "u-1", Email: "u@example.com", Groups: []string{"devs"}}, time.Hour)
	require.NoError(t, err)
	ui, err = e.clientWithToken(tok.Token).UserInfo(context.Background())
	require.NoError(t, err)
	assert.Equal(t, &apiclient.UserInfo{LoggedIn: true, Username: "u@example.com", Iss: auth.Issuer, Groups: []string{"devs"}}, ui)
}

// TestNoRenewalOnSessionEndingRoutes: inside the renewal window every authenticated
// response may carry a replacement login token, except responses that end the session
// (logout) or invalidate it (password change). A renewed token there would outlive the
// action: a CLI persisting renewed tokens would keep a valid 24h session after logout.
func TestNoRenewalOnSessionEndingRoutes(t *testing.T) {
	t.Parallel()
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	intoRenewalWindow := func(tok string) {
		t.Helper()
		p, err := e.tokens.Verify(ctx, tok)
		require.NoError(t, err)
		e.clock.Advance(p.ExpiresAt.Add(-time.Minute).Sub(e.clock.Now()))
	}
	sessionCookies := func(resp *http.Response) []*http.Cookie {
		var out []*http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == auth.SessionCookieName || strings.HasPrefix(c.Name, auth.SessionCookieName+".") {
				out = append(out, c)
			}
		}
		return out
	}

	// Control: a regular request in the window is renewed.
	ctl := e.client("dave")
	intoRenewalWindow(ctl.Token())
	var renewedCtl string
	ctl.OnTokenRenewed(func(tok string) { renewedCtl = tok })
	_, err := ctl.UserInfo(ctx)
	require.NoError(t, err)
	require.True(t, renewedCtl != "", "the renewal window is active")

	// Bearer logout one minute before expiry: no renewed token.
	alice := e.client("alice")
	tok := alice.Token()
	intoRenewalWindow(tok)
	req, _ := http.NewRequest(http.MethodDelete, e.url+apiclient.PathSession, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))
	assert.False(t, resp.Header.Get(apiclient.RenewTokenHeader) != "", "logout must not hand out a replacement session")
	for _, c := range sessionCookies(resp) {
		assert.True(t, c.MaxAge < 0 && c.Value == "", "only clearing cookies on logout, got a %s cookie with max-age %d", c.Name, c.MaxAge)
	}
	_, err = e.tokens.Verify(ctx, tok)
	require.ErrorIs(t, err, auth.ErrTokenRevoked)

	// The Go client's Logout never adopts or reports a token either.
	bob := e.client("admin")
	intoRenewalWindow(bob.Token())
	var reported string
	bob.OnTokenRenewed(func(tok string) { reported = tok })
	_, err = bob.Logout(ctx)
	require.NoError(t, err)
	assert.False(t, reported != "", "no renewed token reported on logout")
	assert.Empty(t, bob.Token())

	// Cookie logout in the window: only the clearing cookie is set.
	b := e.browser()
	resp, err = b.Post(e.url+apiclient.PathSession+"?cookie=true", "application/json",
		strings.NewReader(`{"username":"alice","password":"`+testPassword+`"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))
	cs := sessionCookies(resp)
	require.NotEmpty(t, cs)
	intoRenewalWindow(cs[0].Value)
	req, _ = http.NewRequest(http.MethodDelete, e.url+apiclient.PathSession, nil)
	req.Header.Set(rest.CSRFHeader, rest.CSRFHeaderValue)
	resp, err = b.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, readBody(t, resp))
	cs = sessionCookies(resp)
	require.NotEmpty(t, cs, "logout clears the session cookie")
	for _, c := range cs {
		assert.True(t, c.MaxAge < 0 && c.Value == "", "logout must not set a renewed session cookie, got a %s cookie with max-age %d", c.Name, c.MaxAge)
	}

	// Password change in the window: no replacement token, and the old session stops
	// verifying.
	carol := e.client("alice")
	old := carol.Token()
	intoRenewalWindow(old)
	reported = ""
	carol.OnTokenRenewed(func(tok string) { reported = tok })
	err = carol.UpdatePassword(ctx, &apiclient.UpdatePasswordRequest{CurrentPassword: testPassword, NewPassword: "N3w-password"})
	require.NoError(t, err)
	assert.False(t, reported != "", "a password change must not hand out a replacement session")
	assert.True(t, old == carol.Token(), "the client keeps its (now invalid) token")
	_, err = e.tokens.Verify(ctx, old)
	require.ErrorIs(t, err, auth.ErrSessionInvalidated)
}

// TestCookieOptionsWithoutRequest: rest.HTTPRequest returns nil outside an HTTP handler, and then
// there is no host for an additionalUrls entry to match.
func TestCookieOptionsWithoutRequest(t *testing.T) {
	t.Parallel()
	cm := defaultCM()
	cm["additionalUrls"] = "- https://patrol.example.com\n"
	e := newEnv(t, envOptions{cm: cm})
	got := core.Config{BaseHRef: "/patrol/"}.CookieOptions(nil, e.holder.Current())
	assert.Equal(t, auth.CookieOptions{Path: "/patrol/"}, got)
}
