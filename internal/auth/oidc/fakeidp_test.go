package oidc

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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeIdP is an in-process OpenID provider: discovery, JWKS (RS256 keys generated per
// test), an authorization endpoint that records state/nonce/PKCE challenge and
// immediately redirects back with a code, a token endpoint that enforces redirect_uri,
// client authentication and PKCE (S256), and a userinfo endpoint.
type fakeIdP struct {
	t        *testing.T
	srv      *httptest.Server
	key      *rsa.PrivateKey
	kid      string
	clientID string
	secret   string

	mu sync.Mutex
	// knobs
	now               func() time.Time
	idTokenTTL        time.Duration
	extraClaims       map[string]any
	audience          any // nil: clientID
	nonceOverride     *string
	omitIDToken       bool
	userinfo          map[string]any
	userinfoJWT       bool
	userinfoStatus    int
	discoveryFailures int
	hugeDiscovery     bool
	extraJWKs         []map[string]any // served beside the signing key
	// observations
	discoveryHits int
	tokenRequests []url.Values
	codes         map[string]*authCode
	accessTokens  map[string]string // token -> sub
}

type authCode struct {
	clientID, redirectURI, nonce, challenge, method, sub string
	used                                                 bool
}

func newFakeIdP(t *testing.T) *fakeIdP { return newFakeIdPWith(t, false) }

func newFakeIdPWith(t *testing.T, useTLS bool) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{
		t:            t,
		key:          key,
		kid:          "key-1",
		clientID:     "patrol-web",
		secret:       "web-secret",
		now:          time.Now,
		idTokenTTL:   time.Hour,
		codes:        map[string]*authCode{},
		accessTokens: map[string]string{},
		extraClaims: map[string]any{
			"email":  "jane@example.com",
			"name":   "Jane Doe",
			"groups": []any{"devs", "ops"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /keys", f.jwks)
	mux.HandleFunc("GET /auth", f.authorize)
	mux.HandleFunc("POST /token", f.token)
	mux.HandleFunc("GET /userinfo", f.userinfoHandler)
	if useTLS {
		f.srv = httptest.NewTLSServer(mux)
	} else {
		f.srv = httptest.NewServer(mux)
	}
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) issuer() string { return f.srv.URL }

func (f *fakeIdP) config() Config {
	return Config{Issuer: f.issuer(), ClientID: f.clientID, ClientSecret: f.secret}
}

func (f *fakeIdP) set(fn func(f *fakeIdP)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeIdP) discovery(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.discoveryHits++
	fail := f.discoveryFailures > 0
	if fail {
		f.discoveryFailures--
	}
	huge := f.hugeDiscovery
	f.mu.Unlock()
	if fail {
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	doc := map[string]any{
		"issuer":                                f.issuer(),
		"authorization_endpoint":                f.issuer() + "/auth",
		"token_endpoint":                        f.issuer() + "/token",
		"jwks_uri":                              f.issuer() + "/keys",
		"userinfo_endpoint":                     f.issuer() + "/userinfo",
		"response_types_supported":              []string{"code"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"scopes_supported":                      []string{"openid", "email", "profile", "groups"},
	}
	if huge {
		doc["padding"] = strings.Repeat("x", 2<<20)
	}
	writeJSON(w, doc)
}

func (f *fakeIdP) jwks(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	pub, kid, extra := f.key.PublicKey, f.kid, f.extraJWKs
	f.mu.Unlock()
	keys := []map[string]any{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}
	writeJSON(w, map[string]any{"keys": append(keys, extra...)})
}

func (f *fakeIdP) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != f.clientID || q.Get("response_type") != "code" {
		http.Error(w, "bad client or response type", http.StatusBadRequest)
		return
	}
	if !strings.Contains(" "+q.Get("scope")+" ", " openid ") {
		http.Error(w, "openid scope required", http.StatusBadRequest)
		return
	}
	code := "code-" + randHex(f.t)
	f.mu.Lock()
	f.codes[code] = &authCode{
		clientID: q.Get("client_id"), redirectURI: q.Get("redirect_uri"), nonce: q.Get("nonce"),
		challenge: q.Get("code_challenge"), method: q.Get("code_challenge_method"), sub: "user-123",
	}
	f.mu.Unlock()
	cb, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	cq := cb.Query()
	cq.Set("code", code)
	cq.Set("state", q.Get("state"))
	cb.RawQuery = cq.Encode()
	http.Redirect(w, r, cb.String(), http.StatusFound)
}

func (f *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	form := r.PostForm
	f.mu.Lock()
	f.tokenRequests = append(f.tokenRequests, form)
	f.mu.Unlock()
	id, sec, ok := r.BasicAuth()
	if !ok {
		id, sec = form.Get("client_id"), form.Get("client_secret")
	}
	if id != f.clientID || sec != f.secret {
		tokenError(w, "invalid_client")
		return
	}
	if form.Get("grant_type") != "authorization_code" {
		tokenError(w, "unsupported_grant_type")
		return
	}
	f.mu.Lock()
	ac, ok := f.codes[form.Get("code")]
	if ok && ac.used {
		ok = false
	}
	if ok {
		ac.used = true
	}
	f.mu.Unlock()
	if !ok || ac.redirectURI != form.Get("redirect_uri") {
		tokenError(w, "invalid_grant")
		return
	}
	if ac.challenge != "" {
		verifier := form.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		if ac.method != "S256" || verifier == "" || base64.RawURLEncoding.EncodeToString(sum[:]) != ac.challenge {
			tokenError(w, "invalid_grant")
			return
		}
	}
	access := "at-" + randHex(f.t)
	f.mu.Lock()
	f.accessTokens[access] = ac.sub
	nonce := ac.nonce
	if f.nonceOverride != nil {
		nonce = *f.nonceOverride
	}
	omit := f.omitIDToken
	f.mu.Unlock()
	resp := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600}
	if !omit {
		resp["id_token"] = f.signIDToken(ac.sub, nonce, access, nil)
	}
	writeJSON(w, resp)
}

func (f *fakeIdP) userinfoHandler(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	f.mu.Lock()
	sub, ok := f.accessTokens[tok]
	info := map[string]any{"sub": sub}
	for k, v := range f.userinfo {
		info[k] = v
	}
	asJWT, status := f.userinfoJWT, f.userinfoStatus
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if status != 0 {
		http.Error(w, "nope", status)
		return
	}
	if asJWT {
		claims := jwt.MapClaims{"iss": f.issuer(), "aud": f.clientID}
		for k, v := range info {
			claims[k] = v
		}
		w.Header().Set("Content-Type", "application/jwt")
		_, _ = w.Write([]byte(f.sign(claims)))
		return
	}
	writeJSON(w, info)
}

// signIDToken signs an ID token for sub. override is applied last (nil values delete).
func (f *fakeIdP) signIDToken(sub, nonce, accessToken string, override map[string]any) string {
	f.mu.Lock()
	now := f.now()
	claims := jwt.MapClaims{
		"iss":   f.issuer(),
		"sub":   sub,
		"iat":   now.Unix(),
		"exp":   now.Add(f.idTokenTTL).Unix(),
		"nonce": nonce,
	}
	if f.audience != nil {
		claims["aud"] = f.audience
	} else {
		claims["aud"] = f.clientID
	}
	for k, v := range f.extraClaims {
		claims[k] = v
	}
	f.mu.Unlock()
	if accessToken != "" {
		sum := sha256.Sum256([]byte(accessToken))
		claims["at_hash"] = base64.RawURLEncoding.EncodeToString(sum[:16])
	}
	for k, v := range override {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	return f.sign(claims)
}

func (f *fakeIdP) sign(claims jwt.MapClaims) string {
	f.mu.Lock()
	key, kid := f.key, f.kid
	f.mu.Unlock()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// follow performs the browser leg: GET the authorization URL and return the callback
// parameters from the redirect.
func (f *fakeIdP) follow(authURL string) Callback {
	f.t.Helper()
	client := &http.Client{
		Transport:     f.srv.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(authURL)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		f.t.Fatalf("authorize: status %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		f.t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, loc.String(), nil)
	return CallbackFromRequest(r)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func tokenError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func randHex(t *testing.T) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
