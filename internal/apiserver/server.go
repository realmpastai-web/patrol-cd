// Package apiserver assembles patrol-server's public HTTP API (ADR-0005, ADR-0009,
// docs/architecture/api-contract.md): the route table of internal/apiserver/rest with the
// core routes (internal/apiserver/core) and the resource services registered by other
// packages, the process-wide middleware, the web UI, health and metrics endpoints, TLS and
// graceful shutdown.
package apiserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Service registers routes on the table (resource APIs: applications, projects, ...).
type Service interface {
	Register(t *rest.Table)
}

// ServiceFunc adapts a function to Service.
type ServiceFunc func(t *rest.Table)

// Register calls f.
func (f ServiceFunc) Register(t *rest.Table) { f(t) }

// Defaults of Config.
const (
	DefaultListenAddress   = ":8080"
	DefaultMetricsAddress  = ":8083"
	DefaultMaxBodyBytes    = int64(64 << 20)
	DefaultShutdownTimeout = 30 * time.Second
	// streamDrainGrace is how long in-flight requests may finish after shutdown starts
	// before the base context is canceled, which ends Server-Sent Event streams.
	streamDrainGrace  = 2 * time.Second
	readHeaderTimeout = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 1 << 20
)

// Config is the static configuration of the server (flags / patrol-cmd-params-cm).
type Config struct {
	// Namespace is the control-plane namespace.
	Namespace string
	// ListenAddress serves the API and UI (default :8080).
	ListenAddress string
	// MetricsAddress serves /metrics (default :8083; "-" disables).
	MetricsAddress string
	// Insecure serves plain HTTP (upstream --insecure, e.g. behind a TLS-terminating
	// ingress). Otherwise the port serves HTTPS and answers plain HTTP with a 307
	// redirect to https.
	Insecure bool
	TLS      TLSOptions
	// RootPath mounts the whole server under a sub-path ("/patrol"); /healthz stays
	// available at the root.
	RootPath string
	// BaseHRef is the UI base path injected as <base href> and used for cookie paths
	// (default: RootPath, else "/").
	BaseHRef string
	// XFrameOptions and ContentSecurityPolicy are the header values (nil selects the
	// defaults sameorigin and "default-src 'self'; frame-ancestors 'self'"; "" disables).
	XFrameOptions         *string
	ContentSecurityPolicy *string
	// UI is the web UI (index.html at the root); nil serves no UI.
	UI fs.FS
	// DisableAuth disables authentication and authorization (upstream --disable-auth).
	DisableAuth bool
	// MaxBodyBytes caps request bodies globally (default 64 MiB); routes have lower caps.
	MaxBodyBytes int64
	// ShutdownTimeout bounds graceful shutdown (default 30 s).
	ShutdownTimeout time.Duration
	// TrustedProxies are reverse proxies whose X-Forwarded-For is believed for client IPs
	// (rate limiting, access logs).
	TrustedProxies []netip.Prefix
	// LoginRateLimit configures the per-IP limiter of login routes (zero: defaults).
	LoginRateLimit rest.RateLimiterConfig
	// Core is the configuration of the session/account/settings handlers. Namespace,
	// BaseHRef, DisableAuth and SecureCookies are filled from this Config when empty.
	Core core.Config
}

// SettingsSource provides settings snapshots; *settings.Manager implements it.
type SettingsSource interface {
	Current() *settings.Settings
	Subscribe(fn func(old, new *settings.Settings)) (unsubscribe func())
}

// Deps are the collaborators of the server.
type Deps struct {
	Settings SettingsSource
	// Tokens verifies and issues tokens; its key set follows the settings Secret.
	Tokens *auth.TokenService
	// Login verifies local passwords.
	Login *auth.Authenticator
	// Enforcer evaluates RBAC; its policy follows patrol-rbac-cm.
	Enforcer *rbac.Enforcer
	// Core carries the remaining handler collaborators (Secrets, SSO, RepoServer,
	// Version); Settings, Tokens, Login, Metrics and Logger are filled from Deps.
	Core core.Deps
	// Services are the additional route registrars (resource APIs).
	Services []Service
	// Auditor records audit entries (default: rest.LogAuditor with EventRecorder).
	Auditor rest.Auditor
	// EventRecorder records Kubernetes Events for Application/AppProject audit targets.
	EventRecorder rest.EventRecorder
	// Registry receives the metrics; Gatherer serves them (both default to a new
	// registry).
	Registry prometheus.Registerer
	Gatherer prometheus.Gatherer
	// Ready reports readiness for /healthz (nil: ready once serving).
	Ready  func(ctx context.Context) error
	Logger *slog.Logger
}

// Server is patrol-server's HTTP front end.
type Server struct {
	cfg      Config
	deps     Deps
	log      *slog.Logger
	metrics  *rest.Metrics
	gatherer prometheus.Gatherer
	handler  http.Handler
	table    *rest.Table

	shuttingDown atomic.Bool
	unsubscribe  func()
}

// NewRouteTable returns a table with the core routes (built with zero dependencies) and
// the routes of services: the input of OpenAPI generation.
func NewRouteTable(services ...Service) (*rest.Table, error) {
	t := rest.NewTable()
	core.New(core.Deps{}).Register(t)
	for _, s := range services {
		s.Register(t)
	}
	return t, t.Err()
}

// New builds the server.
func New(cfg Config, deps Deps) (*Server, error) {
	if deps.Settings == nil {
		return nil, errors.New("apiserver: settings source is required")
	}
	if !cfg.DisableAuth && (deps.Tokens == nil || deps.Enforcer == nil) {
		return nil, errors.New("apiserver: token service and RBAC enforcer are required unless authentication is disabled")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	cfg = withDefaults(cfg)
	reg, gatherer := deps.Registry, deps.Gatherer
	if reg == nil {
		r := prometheus.NewRegistry()
		reg, gatherer = r, r
	} else if gatherer == nil {
		if g, ok := reg.(prometheus.Gatherer); ok {
			gatherer = g
		} else {
			gatherer = prometheus.DefaultGatherer
		}
	}
	m, err := rest.NewMetrics(reg)
	if err != nil {
		return nil, fmt.Errorf("apiserver: register metrics: %w", err)
	}
	s := &Server{cfg: cfg, deps: deps, log: deps.Logger, metrics: m, gatherer: gatherer}

	if deps.Enforcer != nil {
		deps.Enforcer.SetEnforcementEnabled(!cfg.DisableAuth)
	}
	if cur := deps.Settings.Current(); cur != nil {
		s.applySettings(cur)
	}
	s.unsubscribe = deps.Settings.Subscribe(func(_, n *settings.Settings) { s.applySettings(n) })

	t, err := s.buildTable()
	if err != nil {
		s.unsubscribe()
		return nil, err
	}
	s.table = t
	if s.cfg.TLS.Certificate == nil {
		src := deps.Settings
		s.cfg.TLS.Certificate = func() *tls.Certificate {
			if st := src.Current(); st != nil {
				return st.ServerCertificate()
			}
			return nil
		}
	}
	h, err := s.buildHandler(t)
	if err != nil {
		s.unsubscribe()
		return nil, err
	}
	s.handler = h
	return s, nil
}

func withDefaults(cfg Config) Config {
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = DefaultListenAddress
	}
	if cfg.MetricsAddress == "" {
		cfg.MetricsAddress = DefaultMetricsAddress
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	cfg.RootPath = normalizeRootPath(cfg.RootPath)
	if cfg.BaseHRef == "" {
		cfg.BaseHRef = cfg.RootPath + "/"
	}
	cfg.BaseHRef = normalizeBaseHRef(cfg.BaseHRef)
	if cfg.Core.Namespace == "" {
		cfg.Core.Namespace = cfg.Namespace
	}
	if cfg.Core.BaseHRef == "" {
		cfg.Core.BaseHRef = cfg.BaseHRef
	}
	cfg.Core.DisableAuth = cfg.Core.DisableAuth || cfg.DisableAuth
	if !cfg.Insecure {
		cfg.Core.SecureCookies = true
	}
	return cfg
}

// normalizeRootPath returns "" or "/x/y".
func normalizeRootPath(p string) string {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return "/" + p
}

// applySettings follows settings changes: RBAC policy and token signing keys.
func (s *Server) applySettings(st *settings.Settings) {
	if st == nil {
		return
	}
	if s.deps.Enforcer != nil {
		r := st.RBAC()
		if err := s.deps.Enforcer.ApplyConfigMap(rbac.BuiltinPolicy(), r.Data); err != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError, "invalid RBAC configuration; keeping the previous policy where possible",
				slog.Any("error", err))
		}
	}
	if s.deps.Tokens != nil {
		ks, err := st.SigningKeys()
		if err != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError, "cannot load session signing keys", slog.Any("error", err))
			return
		}
		if err := s.deps.Tokens.SetKeys(ks); err != nil {
			s.log.LogAttrs(context.Background(), slog.LevelError, "cannot apply session signing keys", slog.Any("error", err))
		}
	}
}

func (s *Server) buildTable() (*rest.Table, error) {
	cd := s.deps.Core
	cd.Config = s.cfg.Core
	cd.Settings = s.deps.Settings
	cd.Tokens = s.deps.Tokens
	cd.Login = s.deps.Login
	cd.Metrics = s.metrics
	cd.Logger = s.log
	t := rest.NewTable()
	core.New(cd).Register(t)
	for _, svc := range s.deps.Services {
		svc.Register(t)
	}
	if err := t.Err(); err != nil {
		return nil, fmt.Errorf("apiserver: route table: %w", err)
	}
	return t, nil
}

func (s *Server) buildHandler(t *rest.Table) (http.Handler, error) {
	limiterCfg := s.cfg.LoginRateLimit
	if limiterCfg.TrustedProxies == nil {
		limiterCfg.TrustedProxies = s.cfg.TrustedProxies
	}
	limiter, err := rest.NewRateLimiter(limiterCfg)
	if err != nil {
		return nil, fmt.Errorf("apiserver: rate limiter: %w", err)
	}
	auditor := s.deps.Auditor
	if auditor == nil {
		auditor = &rest.LogAuditor{Logger: s.log, Recorder: s.deps.EventRecorder, Metrics: s.metrics}
	}
	var enforcer rest.Enforcer
	if s.deps.Enforcer != nil {
		enforcer = s.deps.Enforcer
	}
	var verifier rest.TokenVerifier
	if s.deps.Tokens != nil {
		verifier = s.deps.Tokens
	}
	env := &rest.Env{
		Authenticator: &rest.Authenticator{
			Verifier:    verifier,
			Policy:      s.authPolicy,
			Cookie:      s.cookieOptions,
			DisableAuth: s.cfg.DisableAuth,
			Logger:      s.log,
		},
		Enforcer:     enforcer,
		Auditor:      auditor,
		LoginLimiter: limiter,
		CSRF:         rest.CSRFConfig{TrustedOrigins: s.trustedOrigins},
		Metrics:      s.metrics,
		Logger:       s.log,
		DisableAuth:  s.cfg.DisableAuth,
	}
	mux := http.NewServeMux()
	if err := t.Mount(mux, env); err != nil {
		return nil, fmt.Errorf("apiserver: mount routes: %w", err)
	}
	api := &rest.APIHandler{Mux: mux}
	var ui http.Handler
	if s.cfg.UI != nil {
		u, err := NewUIHandler(s.cfg.UI, s.cfg.BaseHRef, nil)
		if err != nil {
			return nil, fmt.Errorf("apiserver: %w", err)
		}
		ui = u
	}
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			s.healthz(w, r)
		case api.Matches(r):
			api.ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/") || ui == nil:
			rest.NotMatched(mux, w, r)
		default:
			ui.ServeHTTP(w, r)
		}
	})
	var root http.Handler = app
	if s.cfg.RootPath != "" {
		prefixed := http.StripPrefix(s.cfg.RootPath, app)
		root = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/healthz":
				s.healthz(w, r)
			case r.URL.Path == s.cfg.RootPath:
				http.Redirect(w, r, s.cfg.RootPath+"/", http.StatusMovedPermanently)
			case strings.HasPrefix(r.URL.Path, s.cfg.RootPath+"/"):
				prefixed.ServeHTTP(w, r)
			default:
				rest.WriteError(w, r, rest.NotFound("not found (the server is mounted under %s)", s.cfg.RootPath))
			}
		})
	}
	frame, csp := rest.DefaultFrameOptions, rest.DefaultContentSecurityPolicy
	if s.cfg.XFrameOptions != nil {
		frame = *s.cfg.XFrameOptions
	}
	if s.cfg.ContentSecurityPolicy != nil {
		csp = *s.cfg.ContentSecurityPolicy
	}
	return rest.Chain(root,
		rest.Recover(s.log, s.metrics),
		rest.RequestIDs(),
		rest.AccessLog(rest.AccessLogConfig{
			Logger: s.log, QuietPaths: []string{"/healthz", s.cfg.RootPath + "/healthz"},
			ClientIP: func(r *http.Request) string { return rest.ClientIP(r, s.cfg.TrustedProxies) },
		}),
		rest.SecurityHeaders(rest.SecurityHeadersConfig{FrameOptions: frame, ContentSecurityPolicy: csp, HSTS: !s.cfg.Insecure}),
		rest.MaxBody(s.cfg.MaxBodyBytes),
	), nil
}

// authPolicy reads the authentication settings of the current snapshot.
func (s *Server) authPolicy() rest.AuthPolicy {
	st := s.deps.Settings.Current()
	if st == nil {
		return rest.AuthPolicy{Scopes: rbac.DefaultScopes()}
	}
	sc := st.RBAC().Scopes
	if len(sc) == 0 {
		sc = rbac.DefaultScopes()
	}
	return rest.AuthPolicy{AnonymousEnabled: st.AnonymousUserEnabled(), Scopes: sc}
}

// cookieOptions returns the session cookie attributes for r under the current settings.
func (s *Server) cookieOptions(r *http.Request) auth.CookieOptions {
	return s.cfg.Core.CookieOptions(r, s.deps.Settings.Current())
}

// trustedOrigins are url and additionalUrls (CSRF Origin/Referer check).
func (s *Server) trustedOrigins() []string {
	st := s.deps.Settings.Current()
	if st == nil {
		return nil
	}
	var out []string
	if u := st.URL(); u != "" {
		out = append(out, u)
	}
	return append(out, st.AdditionalURLs()...)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if s.shuttingDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("shutting down\n"))
		return
	}
	if s.deps.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.deps.Ready(ctx); err != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "health check failed", slog.Any("error", err))
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready\n"))
			return
		}
	}
	_, _ = w.Write([]byte("ok"))
}

// Handler returns the complete HTTP handler (API, UI, health) with the process-wide
// middleware.
func (s *Server) Handler() http.Handler { return s.handler }

// Routes returns the registered routes.
func (s *Server) Routes() []rest.Route { return s.table.Routes() }

// MetricsHandler serves the Prometheus metrics.
func (s *Server) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(s.gatherer, promhttp.HandlerOpts{ErrorLog: slogPromLogger{s.log}})
}

type slogPromLogger struct{ l *slog.Logger }

func (p slogPromLogger) Println(v ...any) { p.l.Error(fmt.Sprint(v...)) }

// Run serves until ctx is canceled, then shuts down gracefully: /healthz turns 503, new
// connections are refused, in-flight requests get ShutdownTimeout to finish, and
// Server-Sent Event streams are ended after a short grace period.
func (s *Server) Run(ctx context.Context) error {
	defer s.unsubscribe()
	l, err := net.Listen("tcp", s.cfg.ListenAddress)
	if err != nil {
		return fmt.Errorf("apiserver: listen on %s: %w", s.cfg.ListenAddress, err)
	}
	var ml net.Listener
	if s.cfg.MetricsAddress != "-" {
		if ml, err = net.Listen("tcp", s.cfg.MetricsAddress); err != nil {
			_ = l.Close()
			return fmt.Errorf("apiserver: listen on %s: %w", s.cfg.MetricsAddress, err)
		}
	}
	return s.Serve(ctx, l, ml)
}

// Serve is Run on existing listeners (ml may be nil).
func (s *Server) Serve(ctx context.Context, l, ml net.Listener) error {
	baseCtx, cancelBase := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelBase()
	errLog := slog.NewLogLogger(s.log.Handler(), slog.LevelWarn)
	newServer := func(h http.Handler) *http.Server {
		return &http.Server{
			Handler:           h,
			ReadHeaderTimeout: readHeaderTimeout,
			IdleTimeout:       idleTimeout,
			MaxHeaderBytes:    maxHeaderBytes,
			ErrorLog:          errLog,
			BaseContext:       func(net.Listener) context.Context { return baseCtx },
		}
	}
	type served struct {
		srv *http.Server
		l   net.Listener
	}
	var all []served
	main := newServer(s.handler)
	if s.cfg.Insecure {
		all = append(all, served{main, l})
	} else {
		tlsCfg, err := buildTLSConfig(s.cfg.TLS)
		if err != nil {
			_ = l.Close()
			if ml != nil {
				_ = ml.Close()
			}
			return fmt.Errorf("apiserver: TLS: %w", err)
		}
		main.TLSConfig = tlsCfg
		split := newSplitListener(l)
		all = append(all,
			served{main, tls.NewListener(subListener{split, split.tlsCh}, tlsCfg)},
			served{newServer(httpsRedirect()), subListener{split, split.plainCh}},
		)
	}
	if ml != nil {
		mmux := http.NewServeMux()
		mmux.Handle("/metrics", s.MetricsHandler())
		mmux.HandleFunc("/healthz", s.healthz)
		all = append(all, served{newServer(mmux), ml})
	}
	errCh := make(chan error, len(all))
	for _, sv := range all {
		go func() {
			if err := sv.srv.Serve(sv.l); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "patrol-server listening",
		slog.String("address", l.Addr().String()), slog.Bool("tls", !s.cfg.Insecure),
		slog.String("rootPath", s.cfg.RootPath), slog.Bool("authDisabled", s.cfg.DisableAuth))

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
		if runErr == nil {
			runErr = errors.New("apiserver: a listener stopped unexpectedly")
		}
	}
	s.shuttingDown.Store(true)
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ShutdownTimeout)
	defer cancel()
	go func() {
		select {
		case <-time.After(streamDrainGrace):
		case <-shutdownCtx.Done():
		}
		cancelBase()
	}()
	var errs []error
	if runErr != nil {
		errs = append(errs, runErr)
	}
	for _, sv := range all {
		if err := sv.srv.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("apiserver: shutdown: %w", err))
			_ = sv.srv.Close()
		}
	}
	s.log.LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, "patrol-server stopped")
	return errors.Join(errs...)
}
