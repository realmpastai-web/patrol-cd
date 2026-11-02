// Package core implements the session, SSO (OIDC login, callback, CLI exchange, Dex
// proxy), account, settings and version routes of patrol-server (api-contract.md,
// "Session, account, settings, version"), on top of internal/apiserver/rest.
package core

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// SettingsSource returns the current settings snapshot; *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
}

// Config is the static configuration of the handlers.
type Config struct {
	// Namespace is the control-plane namespace.
	Namespace string
	// BaseHRef is the UI base path ("/" or "/patrol/"); cookies use it as their path and
	// SSO redirects land under it.
	BaseHRef string
	// SecureCookies sets the Secure attribute on cookies (false only for plain-HTTP
	// servers; cookies are always Secure when the settings url is https).
	SecureCookies bool
	// DisableAuth mirrors the server's --disable-auth.
	DisableAuth bool
	// AppsInAnyNamespaceEnabled, HydratorEnabled and SyncWithReplaceAllowed are exposed
	// by GET /settings (server flags upstream).
	AppsInAnyNamespaceEnabled bool
	HydratorEnabled           bool
	SyncWithReplaceAllowed    bool
	// CallbackPath is the OIDC redirect path appended to the settings url (default
	// DefaultCallbackPath).
	CallbackPath string
	// ExchangeMaxAge bounds the age of ID tokens accepted by POST
	// /session/oidc-exchange (default DefaultExchangeMaxAge).
	ExchangeMaxAge time.Duration
}

// Deps are the collaborators of the handlers. Nil collaborators make the routes that
// need them fail with 503, so a Handlers built with zero Deps can still register the
// routes (OpenAPI generation).
type Deps struct {
	Config   Config
	Settings SettingsSource
	// Tokens issues and verifies Patrol tokens.
	Tokens *auth.TokenService
	// Login verifies local passwords with throttling.
	Login *auth.Authenticator
	// Secrets updates patrol-secret (passwords, API token metadata).
	Secrets SecretStore
	// SSO runs the OIDC relying party (nil: SSO unavailable).
	SSO *SSO
	// RepoServer lists config management plugins.
	RepoServer apiclient.Client
	// Version reports build and tool versions.
	Version *VersionInfo
	Metrics *rest.Metrics
	Logger  *slog.Logger
	Now     func() time.Time
}

// Handlers serves the core routes.
type Handlers struct {
	d Deps
}

// New returns handlers using d.
func New(d Deps) *Handlers {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Config.CallbackPath == "" {
		d.Config.CallbackPath = DefaultCallbackPath
	}
	if d.Config.ExchangeMaxAge <= 0 {
		d.Config.ExchangeMaxAge = DefaultExchangeMaxAge
	}
	if d.Config.BaseHRef == "" {
		d.Config.BaseHRef = "/"
	}
	return &Handlers{d: d}
}

var errNotConfigured = errors.New("not configured")

func (h *Handlers) settings() (*settings.Settings, error) {
	if h.d.Settings == nil {
		return nil, rest.Unavailable("settings are not available").WithCause(errNotConfigured)
	}
	s := h.d.Settings.Current()
	if s == nil {
		return nil, rest.Unavailable("settings are not loaded yet")
	}
	return s, nil
}

// CookieOptions returns the attributes of the session cookies on the response to r, for login,
// renewal and clearing alike. Behind a TLS-terminating proxy the server sees plain HTTP, so an
// https settings url sets Secure on every response, and an https additionalUrls entry on the
// responses to its host name. s may be nil.
func (c Config) CookieOptions(r *http.Request, s *settings.Settings) auth.CookieOptions {
	secure := c.SecureCookies || (r != nil && r.TLS != nil)
	if s != nil && (strings.HasPrefix(strings.ToLower(s.URL()), "https://") || httpsAdditionalURL(r, s)) {
		secure = true
	}
	return auth.CookieOptions{Path: c.BaseHRef, Secure: secure}
}

// httpsAdditionalURL reports whether additionalUrls has an https entry for the host name of r,
// whatever else is configured for that name. Ports are left out of the comparison: browsers omit
// a default port from Host and some proxies drop any port, and a Secure cookie on a plain HTTP
// port of that name fails where the operator sees it, unlike a missing Secure. A trailing dot on
// the request host names the same host. An entry that does not parse only draws a settings
// warning, so it is skipped here.
func httpsAdditionalURL(r *http.Request, s *settings.Settings) bool {
	if r == nil {
		return false
	}
	host := strings.TrimSuffix((&url.URL{Host: r.Host}).Hostname(), ".")
	return slices.ContainsFunc(s.AdditionalURLs(), func(entry string) bool {
		u, err := url.Parse(entry)
		return err == nil && u.Scheme == "https" && strings.EqualFold(u.Hostname(), host)
	})
}

// cookieOptions returns the session cookie attributes for r.
func (h *Handlers) cookieOptions(r *http.Request, s *settings.Settings) auth.CookieOptions {
	return h.d.Config.CookieOptions(r, s)
}

func (h *Handlers) log() *slog.Logger { return h.d.Logger }

func (h *Handlers) now() time.Time { return h.d.Now() }

// scopes returns the RBAC scopes of s (claims evaluated as groups).
func scopes(s *settings.Settings) []string { return s.RBAC().Scopes }

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, d)
}
