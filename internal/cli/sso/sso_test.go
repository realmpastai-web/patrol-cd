package sso_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/cli/clitest"
	"github.com/juliosaraiva/patrol-cd/internal/cli/sso"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

func provider(idp *clitest.FakeIdP) *sso.Provider {
	return &sso.Provider{Issuer: idp.URL, ClientID: idp.CLIClientID, Scopes: []string{"openid", "email", "offline_access"}}
}

// syncBuffer is written by the login flow and read by the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestLoginAndRefresh(t *testing.T) {
	t.Parallel()
	idp := clitest.NewFakeIdP(t)
	var out syncBuffer
	res, err := sso.Login(t.Context(), provider(idp), sso.LoginOptions{
		LaunchBrowser: true, OpenBrowser: clitest.Browse, Out: &out, Timeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IDToken == "" || res.RefreshToken == "" || res.AccessToken == "" {
		t.Fatal("missing tokens")
	}
	if s := res.String(); strings.Contains(s, res.IDToken) || strings.Contains(s, res.RefreshToken) {
		t.Fatal("Result.String leaks tokens")
	}
	o := out.String()
	for _, w := range []string{"Performing authorization_code flow login: " + idp.URL + "/auth?", "code_challenge_method=S256", "Opening system default browser for authentication", "Authentication successful"} {
		if !strings.Contains(o, w) {
			t.Fatalf("output lacks %q:\n%s", w, o)
		}
	}
	if strings.Contains(o, res.IDToken) {
		t.Fatal("output leaks the ID token")
	}

	ref, err := sso.Refresh(t.Context(), provider(idp), nil, res.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if ref.IDToken == "" || ref.RefreshToken == res.RefreshToken {
		t.Fatalf("refresh: %+v", ref)
	}
	// Refresh tokens are single use at the fake IdP.
	if _, err := sso.Refresh(t.Context(), provider(idp), nil, res.RefreshToken); err == nil {
		t.Fatal("reused refresh token accepted")
	}
	if _, err := sso.Refresh(t.Context(), provider(idp), nil, ""); err == nil {
		t.Fatal("empty refresh token accepted")
	}
}

func TestLoginWithoutBrowser(t *testing.T) {
	t.Parallel()
	idp := clitest.NewFakeIdP(t)
	r, w := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The user copies the URL into a browser.
		buf := make([]byte, 64<<10)
		var all strings.Builder
		re := regexp.MustCompile(`copy-and-paste the following URL into your preferred browser: (\S+)`)
		for {
			n, err := r.Read(buf)
			all.Write(buf[:n])
			if m := re.FindStringSubmatch(all.String()); m != nil {
				_ = clitest.Browse(m[1])
				_, _ = io.Copy(io.Discard, r)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	res, err := sso.Login(t.Context(), provider(idp), sso.LoginOptions{LaunchBrowser: false, Out: w, Timeout: 20 * time.Second})
	_ = w.Close()
	<-done
	if err != nil || res.IDToken == "" {
		t.Fatalf("login: %v", err)
	}
}

func TestLoginCallbackErrors(t *testing.T) {
	t.Parallel()
	idp := clitest.NewFakeIdP(t)
	cases := map[string]struct {
		mutate func(q url.Values)
		want   string
	}{
		"wrong state":    {func(q url.Values) { q.Set("state", "forged"); q.Set("code", "x") }, "Unknown state nonce"},
		"missing code":   {func(q url.Values) { q.Set("state", q.Get("_state")); q.Del("code") }, "no code in request"},
		"provider error": {func(q url.Values) { q.Set("error", "access_denied"); q.Set("error_description", "nope") }, "access_denied: nope"},
		"bad code":       {func(q url.Values) { q.Set("state", q.Get("_state")); q.Set("code", "forged") }, "exchange the authorization code"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The "browser" calls the callback itself with a manipulated query.
			open := func(authURL string) error {
				u, err := url.Parse(authURL)
				if err != nil {
					return err
				}
				redirect := u.Query().Get("redirect_uri")
				q := url.Values{"_state": {u.Query().Get("state")}}
				tc.mutate(q)
				q.Del("_state")
				go func() {
					resp, err := http.Get(redirect + "?" + q.Encode()) //nolint:gosec,noctx // test browser
					if err == nil {
						_ = resp.Body.Close()
					}
				}()
				return nil
			}
			_, err := sso.Login(t.Context(), provider(idp), sso.LoginOptions{LaunchBrowser: true, OpenBrowser: open, Timeout: 20 * time.Second})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestLoginKeySetRefused: the CLI verifies the ID token itself, so a key set go-jose
// refuses stops a login and a session renewal here, with an error that holds the message
// docs/guides/installation.md quotes.
func TestLoginKeySetRefused(t *testing.T) {
	t.Parallel()
	idp := clitest.NewFakeIdP(t)
	opts := sso.LoginOptions{LaunchBrowser: true, OpenBrowser: clitest.Browse, Timeout: 20 * time.Second}
	res, err := sso.Login(t.Context(), provider(idp), opts)
	if err != nil {
		t.Fatal(err)
	}
	idp.SetExtraJWK(map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "unused", "e": "AQAB",
		"n": base64.RawURLEncoding.EncodeToString(new(big.Int).Lsh(big.NewInt(1), 4096).Bytes()), // 4097 bits
	})
	_, loginErr := sso.Login(t.Context(), provider(idp), opts)
	_, refreshErr := sso.Refresh(t.Context(), provider(idp), nil, res.RefreshToken)
	want := "go-jose/go-jose: invalid RSA key, modulus too large"
	for name, err := range map[string]error{"login": loginErr, "refresh": refreshErr} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
	}

	// The refresh token was redeemed before the ID token was verified, so on an IdP that
	// rotates them fixing the key set does not bring the session back: only a login does.
	idp.SetExtraJWK(nil)
	if _, err := sso.Refresh(t.Context(), provider(idp), nil, res.RefreshToken); err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("refresh after the fix: err = %v, want invalid_grant", err)
	}
	if _, err := sso.Login(t.Context(), provider(idp), opts); err != nil {
		t.Errorf("login after the fix: %v", err)
	}
}

func TestLoginTimeoutAndCancel(t *testing.T) {
	t.Parallel()
	idp := clitest.NewFakeIdP(t)
	never := func(string) error { return nil }
	_, err := sso.Login(t.Context(), provider(idp), sso.LoginOptions{LaunchBrowser: true, OpenBrowser: never, Timeout: 200 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := sso.Login(ctx, provider(idp), sso.LoginOptions{OpenBrowser: never}); err == nil {
		t.Fatal("canceled login succeeded")
	}
	// An unreachable issuer fails discovery.
	if _, err := sso.Login(t.Context(), &sso.Provider{Issuer: "http://127.0.0.1:1", ClientID: "x"}, sso.LoginOptions{Timeout: time.Second}); err == nil {
		t.Fatal("unreachable issuer accepted")
	}
}

func TestProviderFromSettings(t *testing.T) {
	t.Parallel()
	if _, err := sso.ProviderFromSettings("cd.example.com", &apiclient.Settings{}); err == nil || err.Error() != "cd.example.com is not configured with SSO" {
		t.Fatalf("no SSO: %v", err)
	}
	p, err := sso.ProviderFromSettings("cd.example.com", &apiclient.Settings{OIDCConfig: &apiclient.OIDCConfig{Issuer: "https://idp.example.com", ClientID: "web", CLIClientID: "cli", Scopes: []string{"email"}}})
	if err != nil || p.ClientID != "cli" || p.Scopes[0] != "openid" || p.Dex {
		t.Fatalf("oidc: %+v %v", p, err)
	}
	p, err = sso.ProviderFromSettings("cd.example.com", &apiclient.Settings{OIDCConfig: &apiclient.OIDCConfig{Issuer: "https://idp.example.com", ClientID: "web"}})
	if err != nil || p.ClientID != "web" {
		t.Fatalf("oidc without CLI client: %+v %v", p, err)
	}
	p, err = sso.ProviderFromSettings("https://cd.example.com", &apiclient.Settings{URL: "https://cd.example.com/", DexConfig: &apiclient.DexConfig{Connectors: []apiclient.DexConnector{{Name: "GitHub", Type: "github"}}}})
	if err != nil || p.Issuer != "https://cd.example.com/api/dex" || p.ClientID != sso.DexCLIClientID || !p.Dex {
		t.Fatalf("dex: %+v %v", p, err)
	}
}

func TestSameHost(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		url, host string
		want      bool
	}{
		{"https://cd.example.com/api/dex", "cd.example.com", true},
		{"https://CD.example.com/api/dex", "cd.example.com", true},
		{"https://cd.example.com:8443/api/dex", "cd.example.com", false},
		{"https://idp.example.com", "cd.example.com", false},
		{"://bad", "cd.example.com", false},
	} {
		if got := sso.SameHost(tc.url, tc.host); got != tc.want {
			t.Fatalf("SameHost(%q, %q) = %v", tc.url, tc.host, got)
		}
	}
}
