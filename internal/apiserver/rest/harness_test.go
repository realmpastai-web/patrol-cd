package rest_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// clock is a settable time source.
type clock struct{ ns atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.ns.Store(time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *clock) Now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *clock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// accountStore is a mutable auth.AccountLookup.
type accountStore struct {
	mu sync.Mutex
	m  auth.Accounts
}

func (s *accountStore) LookupAccount(ctx context.Context, name string) (auth.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m.LookupAccount(ctx, name)
}

func (s *accountStore) update(name string, fn func(*auth.Account)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.m[name]
	fn(&a)
	s.m[name] = a
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

func (a *auditLog) all() []rest.AuditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]rest.AuditEntry(nil), a.entries...)
}

// harness is a mounted route table with real token verification and RBAC.
type harness struct {
	t         *testing.T
	clock     *clock
	tokens    *auth.TokenService
	revs      *auth.MemoryRevocationStore
	accounts  *accountStore
	projects  sync.Map // jti -> bool
	enforcer  *rbac.Enforcer
	audit     *auditLog
	anonymous atomic.Bool
	table     *rest.Table
	env       *rest.Env
	srv       *httptest.Server
}

type harnessOption func(*harness)

func withDisableAuth() harnessOption {
	return func(h *harness) { h.env.DisableAuth = true; h.env.Authenticator.DisableAuth = true }
}

func withLimiter(l *rest.RateLimiter) harnessOption {
	return func(h *harness) { h.env.LoginLimiter = l }
}

const userPolicy = `
p, role:viewer, accounts, get, *, allow
g, alice, role:viewer
p, proj:proj:ci, applications, get, proj/*, allow
p, bob, applications, delete, proj/allowed, allow
`

func newHarness(t *testing.T, register func(t *rest.Table), opts ...harnessOption) *harness {
	t.Helper()
	h := &harness{t: t, clock: newClock(), audit: &auditLog{}}
	ks, err := auth.NewKeySet([]byte(strings.Repeat("k", 32)))
	require.NoError(t, err)
	hash, err := auth.HashPassword("password1")
	require.NoError(t, err)
	h.accounts = &accountStore{m: auth.Accounts{
		"admin": {Name: "admin", Enabled: true, Capabilities: []auth.Capability{auth.CapabilityLogin}, PasswordHash: hash},
		"alice": {Name: "alice", Enabled: true, Capabilities: []auth.Capability{auth.CapabilityLogin, auth.CapabilityAPIKey}, PasswordHash: hash},
		"bob":   {Name: "bob", Enabled: true, Capabilities: []auth.Capability{auth.CapabilityLogin}, PasswordHash: hash},
		"ci":    {Name: "ci", Enabled: true, Capabilities: []auth.Capability{auth.CapabilityAPIKey}},
	}}
	h.revs = auth.NewMemoryRevocationStore(auth.RevocationLimits{}, h.clock.Now)
	h.tokens, err = auth.NewTokenService(auth.TokenServiceConfig{
		Keys: ks, Accounts: h.accounts, Revocations: h.revs, Now: h.clock.Now,
		Projects: auth.TokenRegistryFunc(func(_ context.Context, _, jti string, _ int64) (bool, error) {
			_, ok := h.projects.Load(jti)
			return ok, nil
		}),
	})
	require.NoError(t, err)
	h.enforcer = rbac.NewEnforcer()
	require.NoError(t, h.enforcer.SetPolicy(rbac.BuiltinPolicy(), userPolicy, "", rbac.GlobMatchMode))

	h.table = rest.NewTable()
	register(h.table)
	require.NoError(t, h.table.Err())
	h.env = &rest.Env{
		Authenticator: &rest.Authenticator{
			Verifier: h.tokens,
			Policy: func() rest.AuthPolicy {
				return rest.AuthPolicy{AnonymousEnabled: h.anonymous.Load(), Scopes: rbac.DefaultScopes()}
			},
		},
		Enforcer: h.enforcer,
		Auditor:  h.audit,
		CSRF:     rest.CSRFConfig{TrustedOrigins: func() []string { return []string{"https://patrol.example.com"} }},
	}
	for _, o := range opts {
		o(h)
	}
	handler, err := h.table.Handler(h.env)
	require.NoError(t, err)
	h.srv = httptest.NewServer(rest.Chain(handler, rest.Recover(nil, nil), rest.RequestIDs()))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) loginToken(name string) string {
	h.t.Helper()
	a, err := h.accounts.LookupAccount(context.Background(), name)
	require.NoError(h.t, err)
	iss, err := h.tokens.IssueLoginSession(context.Background(), a, time.Hour)
	require.NoError(h.t, err)
	return iss.Token
}

type reqOpt func(*http.Request)

func bearer(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func cookie(tok string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: tok}) }
}

func header(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func jsonBody(r *http.Request) { r.Header.Set("Content-Type", "application/json") }

func (h *harness) do(method, path, body string, opts ...reqOpt) *http.Response {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	require.NoError(h.t, err)
	for _, o := range opts {
		o(req)
	}
	resp, err := h.srv.Client().Do(req)
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeProblem(t *testing.T, resp *http.Response) apiclient.Problem {
	t.Helper()
	require.Equal(t, apiclient.ProblemContentType, resp.Header.Get("Content-Type"))
	var p apiclient.Problem
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&p))
	return p
}

func decodeJSON[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}
