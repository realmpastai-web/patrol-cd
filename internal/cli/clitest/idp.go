package clitest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// FakeIdP is a minimal OpenID provider for SSO tests: discovery, JWKS, an authorization
// endpoint that redirects straight back with a code (no user interaction), and a token
// endpoint serving the authorization_code grant (PKCE S256 enforced for the public CLI
// client) and the refresh_token grant.
type FakeIdP struct {
	t   testing.TB
	srv *httptest.Server
	key *rsa.PrivateKey
	// URL is the issuer.
	URL string
	// ClientID/ClientSecret are the server's client; CLIClientID the CLI's public client.
	ClientID, ClientSecret, CLIClientID string

	mu       sync.Mutex
	claims   map[string]any
	codes    map[string]idpCode
	refresh  map[string]bool
	refreshN int
	authN    int
	extraJWK map[string]any
}

type idpCode struct{ clientID, redirectURI, nonce, challenge string }

// NewFakeIdP starts a FakeIdP; the ID tokens carry sub "user-123", email
// jane@example.com and groups [devs].
func NewFakeIdP(t testing.TB) *FakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &FakeIdP{
		t: t, key: key, ClientID: "patrol-web", ClientSecret: "clitest-client-secret", CLIClientID: "patrol-cli-app",
		claims:  map[string]any{"email": "jane@example.com", "name": "Jane", "groups": []any{"devs"}},
		codes:   map[string]idpCode{},
		refresh: map[string]bool{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{
			"issuer": f.URL, "authorization_endpoint": f.URL + "/auth", "token_endpoint": f.URL + "/token",
			"jwks_uri": f.URL + "/keys", "response_types_supported": []string{"code"},
			"id_token_signing_alg_values_supported": []string{"RS256"}, "subject_types_supported": []string{"public"},
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
	mux.HandleFunc("GET /auth", f.authorize)
	mux.HandleFunc("POST /token", f.token)
	f.srv = httptest.NewServer(mux)
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

// OIDCConfig is the oidc.config entry of patrol-cm for this provider.
func (f *FakeIdP) OIDCConfig(extra string) string {
	return "name: Fake\nissuer: " + f.URL + "\nclientID: " + f.ClientID + "\nclientSecret: " + f.ClientSecret +
		"\ncliClientID: " + f.CLIClientID + "\nrequestedScopes: [openid, profile, email, groups, offline_access]\n" + extra
}

// SetClaim sets an ID token claim (nil deletes it).
func (f *FakeIdP) SetClaim(k string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v == nil {
		delete(f.claims, k)
		return
	}
	f.claims[k] = v
}

// SetExtraJWK serves key in the key set beside the signing key.
func (f *FakeIdP) SetExtraJWK(key map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.extraJWK = key
}

// Authorizations and Refreshes count the authorization requests and refresh grants.
func (f *FakeIdP) Authorizations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authN
}

// Refreshes counts the refresh_token grants served.
func (f *FakeIdP) Refreshes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshN
}

func (f *FakeIdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cid := q.Get("client_id")
	if cid != f.ClientID && cid != f.CLIClientID {
		http.Error(w, "unknown client", http.StatusBadRequest)
		return
	}
	if cid == f.CLIClientID && (q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256") {
		http.Error(w, "PKCE required", http.StatusBadRequest)
		return
	}
	cb, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || cb.Scheme == "" {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	code := "code-" + f.random()
	f.mu.Lock()
	f.authN++
	f.codes[code] = idpCode{clientID: cid, redirectURI: q.Get("redirect_uri"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	f.mu.Unlock()
	cq := cb.Query()
	cq.Set("code", code)
	cq.Set("state", q.Get("state"))
	cb.RawQuery = cq.Encode()
	http.Redirect(w, r, cb.String(), http.StatusFound)
}

func (f *FakeIdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		tokenError(w, "invalid_request")
		return
	}
	id, sec, ok := r.BasicAuth()
	if !ok {
		id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id == f.ClientID && sec != f.ClientSecret || id != f.ClientID && id != f.CLIClientID {
		tokenError(w, "invalid_client")
		return
	}
	nonce := ""
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		f.mu.Lock()
		c, found := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		if !found || c.clientID != id || c.redirectURI != r.PostForm.Get("redirect_uri") {
			tokenError(w, "invalid_grant")
			return
		}
		if c.challenge != "" {
			sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
				tokenError(w, "invalid_grant")
				return
			}
		}
		nonce = c.nonce
	case "refresh_token":
		rt := r.PostForm.Get("refresh_token")
		f.mu.Lock()
		valid := f.refresh[rt]
		delete(f.refresh, rt)
		if valid {
			f.refreshN++
		}
		f.mu.Unlock()
		if !valid {
			tokenError(w, "invalid_grant")
			return
		}
	default:
		tokenError(w, "unsupported_grant_type")
		return
	}
	rt := "rt-" + f.random()
	f.mu.Lock()
	f.refresh[rt] = true
	f.mu.Unlock()
	claims := map[string]any{"aud": id}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	writeJSON(w, map[string]any{
		"access_token": "at-" + f.random(), "token_type": "Bearer", "expires_in": 3600,
		"refresh_token": rt, "id_token": f.IDToken(claims),
	})
}

// IDToken signs an ID token for user-123 with the configured claims and overrides.
func (f *FakeIdP) IDToken(override map[string]any) string {
	now := time.Now()
	c := jwt.MapClaims{"iss": f.URL, "sub": "user-123", "aud": f.CLIClientID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	f.mu.Lock()
	for k, v := range f.claims {
		c[k] = v
	}
	f.mu.Unlock()
	for k, v := range override {
		c[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(f.key)
	if err != nil {
		f.t.Errorf("sign ID token: %v", err)
	}
	return s
}

func (f *FakeIdP) random() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
