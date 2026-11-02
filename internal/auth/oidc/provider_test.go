package oidc

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/golang-jwt/jwt/v5"

	"github.com/juliosaraiva/patrol-cd/internal/auth"
)

func (f *fakeIdP) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discoveryHits
}

func TestVerifyIDToken(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	ctx := context.Background()
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	now := time.Now().Unix()
	baseClaims := func() jwt.MapClaims {
		return jwt.MapClaims{"iss": f.issuer(), "sub": "u", "aud": f.clientID, "iat": now, "exp": now + 600}
	}
	signWith := func(m jwt.SigningMethod, key any, kid string, c jwt.MapClaims) string {
		tok := jwt.NewWithClaims(m, c)
		tok.Header["kid"] = kid
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		token   func() string
		wantErr error
		expired bool
	}{
		{name: "valid", token: func() string { return f.signIDToken("u", "", "", nil) }},
		{name: "aud as list", token: func() string { return f.signIDToken("u", "", "", map[string]any{"aud": []any{"x", f.clientID}}) }},
		{name: "cli client audience", mutate: func(c *Config) { c.CLIClientID = "patrol-cli" },
			token: func() string { return f.signIDToken("u", "", "", map[string]any{"aud": "patrol-cli"}) }},
		{name: "explicit audiences exclude client id", mutate: func(c *Config) { c.Audiences = []string{"api"} },
			token: func() string { return f.signIDToken("u", "", "", nil) }, wantErr: ErrAudience},
		{name: "explicit audience match", mutate: func(c *Config) { c.Audiences = []string{"api"} },
			token: func() string { return f.signIDToken("u", "", "", map[string]any{"aud": "api"}) }},
		{name: "wrong audience", token: func() string { return f.signIDToken("u", "", "", map[string]any{"aud": "other"}) }, wantErr: ErrAudience},
		{name: "no audience", token: func() string { return f.signIDToken("u", "", "", map[string]any{"aud": nil}) }, wantErr: ErrAudience},
		{name: "no audience allowed by flag", mutate: func(c *Config) { c.SkipAudienceCheckWhenTokenHasNoAudience = true },
			token: func() string { return f.signIDToken("u", "", "", map[string]any{"aud": nil}) }},
		{name: "wrong issuer", token: func() string { return f.signIDToken("u", "", "", map[string]any{"iss": "https://evil.example.com"}) }, wantErr: ErrIDTokenInvalid},
		{name: "expired", token: func() string { return f.signIDToken("u", "", "", map[string]any{"exp": now - 60}) }, wantErr: ErrIDTokenInvalid, expired: true},
		{name: "signed by another key with the same kid", token: func() string { return signWith(jwt.SigningMethodRS256, otherKey, f.kid, baseClaims()) }, wantErr: ErrIDTokenInvalid},
		{name: "HS256 with the IdP public key as secret", token: func() string { return signWith(jwt.SigningMethodHS256, pubPEM, f.kid, baseClaims()) }, wantErr: ErrIDTokenInvalid},
		{name: "alg none", token: func() string {
			return signWith(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, f.kid, baseClaims())
		}, wantErr: ErrIDTokenInvalid},
		{name: "RS512 not advertised", token: func() string { return signWith(jwt.SigningMethodRS512, f.key, f.kid, baseClaims()) }, wantErr: ErrIDTokenInvalid},
		{name: "oversized", token: func() string { return strings.Repeat("a", auth.MaxTokenLength+1) }, wantErr: ErrIDTokenInvalid},
		{name: "garbage", token: func() string { return "a.b.c" }, wantErr: ErrIDTokenInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newTestProvider(t, f, tc.mutate)
			idt, err := p.VerifyIDToken(ctx, tc.token())
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if idt.Subject != "u" {
					t.Fatalf("subject = %q", idt.Subject)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if _, isExp := errors.AsType[*gooidc.TokenExpiredError](err); isExp != tc.expired {
				t.Fatalf("expired error = %v, want %v (%v)", isExp, tc.expired, err)
			}
		})
	}
}

func TestVerifyExternal(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	ctx := context.Background()
	p := newTestProvider(t, f, nil)

	raw := f.signIDToken("dex-sub", "", "", map[string]any{
		"jti":              "id-1",
		"federated_claims": map[string]any{"user_id": "jdoe"},
	})
	pr, err := p.VerifyExternal(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Subject != "jdoe" || pr.Kind != auth.KindExternal || pr.TokenID != "id-1" || pr.Issuer != f.issuer() ||
		pr.Email != "jane@example.com" || !slices.Equal(pr.Groups, []string{"devs", "ops"}) || pr.ExpiresAt.IsZero() {
		t.Fatalf("principal = %+v", pr)
	}
	if pr.Username() != "jane@example.com" {
		t.Fatalf("username = %q", pr.Username())
	}
	// at_hash is the revocation id when there is no jti/uti (Dex).
	pr2, err := p.VerifyExternal(ctx, f.signIDToken("u", "", "access-token", nil))
	if err != nil || pr2.TokenID == "" {
		t.Fatalf("at_hash id: %+v %v", pr2, err)
	}
	pr3, err := p.VerifyExternal(ctx, f.signIDToken("u", "", "", map[string]any{"uti": "entra-uti"}))
	if err != nil || pr3.TokenID != "entra-uti" {
		t.Fatalf("uti id: %+v %v", pr3, err)
	}

	_, err = p.VerifyExternal(ctx, f.signIDToken("u", "", "", map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}))
	if !errors.Is(err, auth.ErrInvalidToken) || !errors.Is(err, auth.ErrTokenExpired) {
		t.Fatalf("expired: %v", err)
	}
	_, err = p.VerifyExternal(ctx, f.signIDToken("u", "", "", map[string]any{"aud": "nope"}))
	if !errors.Is(err, auth.ErrInvalidToken) || !errors.Is(err, ErrAudience) {
		t.Fatalf("audience: %v", err)
	}

	// Through auth.TokenService: routed by issuer, revocable by token id.
	ks, _ := auth.NewKeySet(serverSecret)
	revs := auth.NewMemoryRevocationStore(auth.RevocationLimits{}, nil)
	svc, err := auth.NewTokenService(auth.TokenServiceConfig{Keys: ks, Accounts: auth.Accounts{}, Revocations: revs, External: p})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Verify(ctx, raw)
	if err != nil || got.Subject != "jdoe" {
		t.Fatalf("service verify: %+v %v", got, err)
	}
	if err := svc.Revoke(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Verify(ctx, raw); !errors.Is(err, auth.ErrTokenRevoked) {
		t.Fatalf("revoked external token: %v", err)
	}
}

func TestVerifyExternalUserInfoGroupsCache(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	clock := newTestClock()
	f.set(func(f *fakeIdP) {
		f.now = clock.Now
		f.userinfo = map[string]any{"groups": []any{"ui-group"}}
	})
	p := newTestProvider(t, f, func(c *Config) {
		c.EnableUserInfoGroups = true
		c.UserInfoCacheExpiration = time.Minute
	}, WithClock(clock.Now))
	ctx := context.Background()
	raw := f.signIDToken("user-123", "", "", nil)

	// No login yet: no access token to query userinfo with, fail closed.
	if _, err := p.VerifyExternal(ctx, raw); !errors.Is(err, ErrUserInfoUnavailable) || !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("before login: %v", err)
	}
	codec := newTestCodec(t, clock.Now)
	authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.HandleCallback(ctx, f.follow(authURL), cookie, codec); err != nil {
		t.Fatal(err)
	}
	pr, err := p.VerifyExternal(ctx, raw)
	if err != nil || !slices.Equal(pr.Groups, []string{"ui-group"}) {
		t.Fatalf("after login: %+v %v", pr, err)
	}
	clock.Advance(time.Minute + time.Second)
	if _, err := p.VerifyExternal(ctx, raw); !errors.Is(err, ErrUserInfoUnavailable) {
		t.Fatalf("after cache expiry: %v", err)
	}
}

func TestLazyDiscoveryAndRetry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("lazy", func(t *testing.T) {
		t.Parallel()
		f := newFakeIdP(t)
		_ = newTestProvider(t, f, nil)
		if f.hits() != 0 {
			t.Fatal("NewProvider contacted the IdP")
		}
	})
	t.Run("retries transient failures", func(t *testing.T) {
		t.Parallel()
		f := newFakeIdP(t)
		f.set(func(f *fakeIdP) { f.discoveryFailures = 2 })
		p := newTestProvider(t, f, nil, WithDiscoveryRetry(3, time.Millisecond))
		if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); err != nil {
			t.Fatal(err)
		}
		if f.hits() != 3 {
			t.Fatalf("hits = %d", f.hits())
		}
		// Memoized afterwards.
		if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); err != nil || f.hits() != 3 {
			t.Fatalf("second call: %v hits=%d", err, f.hits())
		}
	})
	t.Run("gives up and cools down", func(t *testing.T) {
		t.Parallel()
		f := newFakeIdP(t)
		f.set(func(f *fakeIdP) { f.discoveryFailures = 100 })
		p := newTestProvider(t, f, nil, WithDiscoveryRetry(2, time.Millisecond), WithFailureCooldown(time.Hour))
		if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); !errors.Is(err, ErrDiscovery) {
			t.Fatalf("err = %v", err)
		}
		if f.hits() != 2 {
			t.Fatalf("hits = %d", f.hits())
		}
		if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); !errors.Is(err, ErrDiscovery) || f.hits() != 2 {
			t.Fatalf("cooldown not honored: %v hits=%d", err, f.hits())
		}
	})
	t.Run("recovers after cooldown", func(t *testing.T) {
		t.Parallel()
		f := newFakeIdP(t)
		f.set(func(f *fakeIdP) { f.discoveryFailures = 1 })
		p := newTestProvider(t, f, nil) // 1 attempt, no cooldown
		if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); !errors.Is(err, ErrDiscovery) {
			t.Fatalf("first: %v", err)
		}
		if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); err != nil {
			t.Fatalf("second: %v", err)
		}
	})
	t.Run("caller cancellation", func(t *testing.T) {
		t.Parallel()
		f := newFakeIdP(t)
		f.set(func(f *fakeIdP) { f.discoveryFailures = 100 })
		p := newTestProvider(t, f, nil, WithDiscoveryRetry(50, 50*time.Millisecond))
		cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if _, err := p.BuildAuthURL(cctx, "s", "n", "", callbackURL); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("oversized discovery document", func(t *testing.T) {
		t.Parallel()
		f := newFakeIdP(t)
		f.set(func(f *fakeIdP) { f.hugeDiscovery = true })
		p := newTestProvider(t, f, nil)
		_, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL)
		if !errors.Is(err, ErrDiscovery) || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestIdPKeyRotationAndRediscoveryRateLimit(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	ctx := context.Background()
	p := newTestProvider(t, f, nil)
	if _, err := p.VerifyIDToken(ctx, f.signIDToken("u", "", "", nil)); err != nil {
		t.Fatal(err)
	}
	// The IdP rotates its signing key: the JWKS is refetched transparently.
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.set(func(f *fakeIdP) { f.key, f.kid = newKey, "key-2" })
	if _, err := p.VerifyIDToken(ctx, f.signIDToken("u", "", "", nil)); err != nil {
		t.Fatalf("after rotation: %v", err)
	}
	// Signature failures trigger at most one rediscovery per interval.
	before := f.hits()
	attacker, _ := rsa.GenerateKey(rand.Reader, 2048)
	forge := func() string {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": f.issuer(), "sub": "u", "aud": f.clientID, "exp": time.Now().Add(time.Hour).Unix()})
		tok.Header["kid"] = "key-2"
		s, _ := tok.SignedString(attacker)
		return s
	}
	for range 3 {
		if _, err := p.VerifyIDToken(ctx, forge()); !errors.Is(err, ErrIDTokenInvalid) {
			t.Fatalf("forged: %v", err)
		}
	}
	if got := f.hits() - before; got != 1 {
		t.Fatalf("rediscoveries = %d, want 1", got)
	}
}

// rsaJWK is an RSA public key whose modulus has the given length. It signs nothing, so
// the modulus need not be a product of two primes.
func rsaJWK(bits int) map[string]any {
	n := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	return map[string]any{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "unused",
		"n": base64.RawURLEncoding.EncodeToString(n.SetBit(n, 0, 1).Bytes()),
		"e": "AQAB",
	}
}

func ed25519JWK(x []byte) map[string]any {
	return map[string]any{
		"kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "kid": "unused",
		"x": base64.RawURLEncoding.EncodeToString(x),
	}
}

// TestKeySetLimits pins what go-jose refuses while parsing the IdP's key set: one such
// key fails every verification, although a good key signed the token. The limits are
// kept on purpose (#273), and these are the messages docs/guides/installation.md quotes.
func TestKeySetLimits(t *testing.T) {
	t.Parallel()
	goodEd25519, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const (
		tooLarge    = "go-jose/go-jose: invalid RSA key, modulus too large"
		wrongLength = "go-jose/go-jose: invalid Ed25519 public key, wrong length for x"
		lowOrder    = "go-jose/go-jose: invalid Ed25519 public key, low-order point"
	)
	tests := []struct {
		name    string
		key     map[string]any
		wantErr string
	}{
		{name: "RSA key of 4096 bits", key: rsaJWK(4096)},
		{name: "RSA key of 4097 bits", key: rsaJWK(4097), wantErr: tooLarge},
		{name: "RSA key of 8192 bits", key: rsaJWK(8192), wantErr: tooLarge},
		{name: "valid Ed25519 key", key: ed25519JWK(goodEd25519)},
		{name: "Ed25519 key of 31 bytes", key: ed25519JWK(make([]byte, ed25519.PublicKeySize-1)), wantErr: wrongLength},
		{name: "Ed25519 key of 33 bytes", key: ed25519JWK(make([]byte, ed25519.PublicKeySize+1)), wantErr: wrongLength},
		{name: "Ed25519 low-order point", key: ed25519JWK(make([]byte, ed25519.PublicKeySize)), wantErr: lowOrder},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeIdP(t)
			f.set(func(f *fakeIdP) { f.extraJWKs = []map[string]any{tc.key} })
			p := newTestProvider(t, f, nil)
			ctx := context.Background()
			raw := f.signIDToken("u", "", "", nil)

			_, verifyErr := p.VerifyIDToken(ctx, raw)
			_, externalErr := p.VerifyExternal(ctx, raw)
			codec := newTestCodec(t, time.Now)
			// Discovery does not fetch the key set, so a login only fails at the callback.
			authURL, cookie, err := p.StartLogin(ctx, codec, LoginRequest{RedirectURL: callbackURL})
			if err != nil {
				t.Fatal(err)
			}
			_, callbackErr := p.HandleCallback(ctx, f.follow(authURL), cookie, codec)

			for name, err := range map[string]error{"VerifyIDToken": verifyErr, "VerifyExternal": externalErr, "HandleCallback": callbackErr} {
				if tc.wantErr == "" {
					if err != nil {
						t.Errorf("%s: %v", name, err)
					}
					continue
				}
				if !errors.Is(err, ErrIDTokenInvalid) || !strings.Contains(fmt.Sprint(err), tc.wantErr) {
					t.Errorf("%s: err = %v, want %q", name, err, tc.wantErr)
				}
			}
			if tc.wantErr != "" && !errors.Is(externalErr, auth.ErrInvalidToken) {
				t.Errorf("VerifyExternal: err = %v, want auth.ErrInvalidToken", externalErr)
			}
		})
	}
}

func TestProviderTLSConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeIdPWith(t, true)
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}))
	build := func(mut func(*Config)) (*Provider, error) {
		cfg := f.config()
		mut(&cfg)
		return NewProvider(cfg, WithLogger(discardLogger), WithDiscoveryRetry(1, 0), WithFailureCooldown(0))
	}

	p, err := build(func(c *Config) { c.RootCA = caPEM })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); err != nil {
		t.Fatalf("with rootCA: %v", err)
	}
	p, _ = build(func(*Config) {})
	if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("untrusted CA accepted: %v", err)
	}
	p, _ = build(func(c *Config) { c.InsecureSkipVerify = true })
	if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); err != nil {
		t.Fatalf("insecureSkipVerify: %v", err)
	}
	if _, err := build(func(c *Config) { c.RootCA = "not a pem" }); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bad rootCA: %v", err)
	}
}

func TestLimitedBody(t *testing.T) {
	t.Parallel()
	read := func(n int, limit int64) error {
		b := &limitedBody{rc: io.NopCloser(bytes.NewReader(make([]byte, n))), remaining: limit}
		_, err := io.ReadAll(b)
		return err
	}
	if err := read(100, 100); err != nil {
		t.Fatalf("exact limit: %v", err)
	}
	if err := read(101, 100); !errors.Is(err, errResponseTooLarge) {
		t.Fatalf("over limit: %v", err)
	}
	if err := read(0, 100); err != nil {
		t.Fatalf("empty: %v", err)
	}
}

func TestFailedRediscoveryKeepsWorkingProvider(t *testing.T) {
	t.Parallel()
	f := newFakeIdP(t)
	clock := newTestClock()
	p := newTestProvider(t, f, nil, WithClock(clock.Now))
	ctx := context.Background()
	valid := f.signIDToken("u", "", "", nil)
	if _, err := p.VerifyIDToken(ctx, valid); err != nil {
		t.Fatal(err)
	}
	f.set(func(f *fakeIdP) { f.discoveryFailures = 100 })
	attacker, _ := rsa.GenerateKey(rand.Reader, 2048)
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"iss": f.issuer(), "sub": "u", "aud": f.clientID, "exp": clock.Now().Add(time.Hour).Unix()})
	tok.Header["kid"] = f.kid
	forged, _ := tok.SignedString(attacker)
	before := f.hits()
	if _, err := p.VerifyIDToken(ctx, forged); !errors.Is(err, ErrIDTokenInvalid) {
		t.Fatalf("forged: %v", err)
	}
	if f.hits() != before+1 {
		t.Fatalf("rediscovery not attempted: hits %d -> %d", before, f.hits())
	}
	// The IdP's discovery endpoint is down, but the previously discovered provider and
	// its keys keep verifying legitimate tokens.
	if _, err := p.VerifyIDToken(ctx, valid); err != nil {
		t.Fatalf("valid token after failed rediscovery: %v", err)
	}
	if _, err := p.BuildAuthURL(ctx, "s", "n", "", callbackURL); err != nil {
		t.Fatalf("auth URL after failed rediscovery: %v", err)
	}
}
