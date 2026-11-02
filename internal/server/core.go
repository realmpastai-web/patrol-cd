package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	krest "k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/core"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applications"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/applicationsets"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/badge"
	clustersvc "github.com/juliosaraiva/patrol-cd/internal/apiserver/services/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/notifications"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/projects"
	"github.com/juliosaraiva/patrol-cd/internal/apiserver/services/repositories"
	"github.com/juliosaraiva/patrol-cd/internal/appset/generators"
	"github.com/juliosaraiva/patrol-cd/internal/auth"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/controller/sharding"
	"github.com/juliosaraiva/patrol-cd/internal/controller/treeapi"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	reposerver "github.com/juliosaraiva/patrol-cd/internal/reposerver/apiclient"
	"github.com/juliosaraiva/patrol-cd/internal/repository"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

// Core mode (`patrol --core`, API-055, docs/architecture/components/cli-core-mode.md):
// NewCoreHandler assembles the patrol-server API inside the CLI process. Unlike Run it opens
// no listener (the CLI calls the returned handler through an in-memory transport), starts
// no metrics or health endpoint, writes nothing at startup (no signing key bootstrap, no
// default project), and makes every Kubernetes call with the caller's own credentials:
// authentication and Patrol RBAC are disabled, so the Kubernetes RBAC of the kubeconfig
// user is the only authorization.

// Components reached through CoreConfig.Dial.
const (
	CoreComponentRepoServer = "repo-server"
	CoreComponentController = "controller"
)

// DefaultCoreSyncTimeout bounds the lazily started repository informers of core mode.
const DefaultCoreSyncTimeout = 30 * time.Second

const (
	// coreIdentityIssuer is the issuer of the per-process token that carries the caller's
	// Kubernetes identity from the handler wrapper to the API middleware.
	coreIdentityIssuer = "patrol-core"
	// coreTokenPlaceholder satisfies the internal clients' non-empty token check; coreBearer
	// replaces it with the internal token on every request.
	coreTokenPlaceholder = "core-mode-internal-token-set-per-request"
	// coreStartupTimeout bounds the startup reads (identity, settings, parameters).
	coreStartupTimeout = 30 * time.Second
	// coreEventTimeout bounds the creation of one audit Event.
	coreEventTimeout = 5 * time.Second
	// coreEventComponent is the source component of audit Events recorded in core mode.
	coreEventComponent = "patrol-cli"
	// maxCoreStatusBytes bounds the Kubernetes Status bodies inspected for diagnostics.
	maxCoreStatusBytes = 1 << 20
	// maxCoreProblemBytes bounds error documents that get diagnostics appended.
	maxCoreProblemBytes = 1 << 20
	// maxCoreDiagnostics and maxCoreDiagnosticBytes bound the diagnostics of a request.
	maxCoreDiagnostics     = 4
	maxCoreDiagnosticBytes = 1 << 10
)

// CoreDialTarget is an internal endpoint the in-process server needs: the repo-server
// (any ready pod) or the controller pod that shard routing selected.
type CoreDialTarget struct {
	// Component is CoreComponentRepoServer or CoreComponentController.
	Component string
	// Pod is the controller pod (shard routing); empty for the repo-server.
	Pod string
	// Port is the container port.
	Port int
}

// CoreConfig configures NewCoreHandler.
type CoreConfig struct {
	// Namespace is the control-plane namespace (settings, projects, clusters,
	// repositories, Applications). Required.
	Namespace string
	// RestConfig carries the caller's kubeconfig credentials. Required. Every Kubernetes
	// call of the handler uses it; it is never replaced by another identity.
	RestConfig *krest.Config
	// Dial connects to the repo-server and controller pods (Kubernetes port-forward with
	// the caller's credentials). Nil dials the configured addresses directly (RepoServerURL
	// and TreeAPI.URL, tests and debugging).
	Dial func(ctx context.Context, target CoreDialTarget) (net.Conn, error)
	// RepoServerURL replaces the repo-server address of patrol-cmd-params-cm (repo.server).
	RepoServerURL string
	// TreeAPI routes controller tree API calls: URL sends every call to one controller,
	// otherwise the replica owning the Application's shard is selected as patrol-server
	// does (StatefulSet, Service, Port; TLS.Plaintext selects http).
	TreeAPI TreeAPIClientOptions
	// InternalToken returns the token of the repo-server and the tree API on first use.
	// Nil reads Secret patrol-internal-token with RestConfig.
	InternalToken func(ctx context.Context) (string, error)
	// DestinationConfig returns the caller's client configuration of a destination cluster
	// for the resource routes (logs, events, live resources, actions), the previews of a
	// cluster whose status lacks its versions, the cluster connection checks and rotate-auth;
	// credentials stored in Cluster Secrets are never used. Nil serves only the in-cluster
	// destination, with RestConfig.
	DestinationConfig func(ctx context.Context, c *v1alpha1.Cluster) (*krest.Config, error)
	// SyncTimeout bounds the lazily started repository informers (default
	// DefaultCoreSyncTimeout).
	SyncTimeout time.Duration
	// OnWarning receives degradations the user should know about (a settings ConfigMap that
	// cannot be read). Nil logs them.
	OnWarning func(msg string)
	// Logger receives the server logs (nil discards them).
	Logger *slog.Logger
}

func (c *CoreConfig) validate() error {
	var errs []error
	if c.Namespace == "" {
		errs = append(errs, errors.New("namespace is required"))
	} else if !literalNamespaceRE.MatchString(c.Namespace) {
		errs = append(errs, fmt.Errorf("namespace %q is not a valid namespace name", c.Namespace))
	}
	if c.RestConfig == nil {
		errs = append(errs, errors.New("a Kubernetes client configuration is required"))
	}
	return errors.Join(errs...)
}

// NewCoreHandler returns the in-process API handler of core mode and the function that
// releases it (lazily started informers, idle connections). Startup reads the caller's
// identity (SelfSubjectReview), the settings objects and patrol-cmd-params-cm once, with
// the caller's credentials; unreadable objects are treated as absent.
func NewCoreHandler(ctx context.Context, cfg CoreConfig) (http.Handler, func(), error) {
	a, err := newCoreAssembly(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	return a.handler, a.close, nil
}

func newCoreAssembly(ctx context.Context, cfg CoreConfig) (*coreAssembly, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid core mode configuration: %w", err)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if cfg.SyncTimeout <= 0 {
		cfg.SyncTimeout = DefaultCoreSyncTimeout
	}
	actx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	a := &coreAssembly{cfg: cfg, log: log, ctx: actx, cancel: cancel}
	if err := a.build(ctx); err != nil {
		a.close()
		return nil, err
	}
	return a, nil
}

// coreAssembly is the dependency graph of the in-process server.
type coreAssembly struct {
	cfg    CoreConfig
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	conns    connTracker
	restCfg  *krest.Config
	hc       *http.Client
	kube     kubernetes.Interface
	dyn      dynamic.Interface
	direct   client.WithWatch
	scheme   *runtime.Scheme
	settings coreSettings
	params   *settings.Params

	identityToken string
	internal      *http.Transport
	repoHostPort  string
	repoPort      int
	treeService   string
	token         *coreTokenCache

	api       *apiserver.Server
	handler   http.Handler
	closeOnce sync.Once
}

func (a *coreAssembly) close() {
	a.closeOnce.Do(func() {
		a.cancel()
		a.wg.Wait()
		a.conns.closeAll()
		if a.hc != nil {
			utilnet.CloseIdleConnectionsFor(a.hc.Transport)
		}
		if a.internal != nil {
			a.internal.CloseIdleConnections()
		}
	})
}

func (a *coreAssembly) warn(msg string) {
	if a.cfg.OnWarning != nil {
		a.cfg.OnWarning(msg)
		return
	}
	a.log.Warn(msg)
}

func (a *coreAssembly) build(ctx context.Context) error {
	cfg := a.cfg
	ns := cfg.Namespace
	rc := krest.CopyConfig(cfg.RestConfig)
	rc.Wrap(func(rt http.RoundTripper) http.RoundTripper { return &coreStatusRecorder{next: rt} })
	// A dial function of its own gives the assembly a dedicated (uncached) transport whose
	// connections close tracks and closes, including streams of informers still ending.
	if rc.Dial == nil {
		rc.Dial = a.conns.dial
	}
	hc, err := krest.HTTPClientFor(rc)
	if err != nil {
		return fmt.Errorf("create Kubernetes HTTP client: %w", err)
	}
	a.restCfg, a.hc = rc, hc
	if a.scheme, err = newScheme(); err != nil {
		return err
	}
	if a.kube, err = kubernetes.NewForConfigAndClient(rc, hc); err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	if a.dyn, err = dynamic.NewForConfigAndClient(rc, hc); err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}
	if a.direct, err = client.NewWithWatch(rc, client.Options{Scheme: a.scheme, HTTPClient: hc}); err != nil {
		return fmt.Errorf("create client: %w", err)
	}

	sctx, cancel := context.WithTimeout(ctx, coreStartupTimeout)
	defer cancel()
	who := a.whoAmI(sctx)
	st, err := loadCoreSettings(sctx, a.kube, ns, a.warn, a.log)
	if err != nil {
		return err
	}
	a.settings = coreSettings{st: st}
	cm, err := readCmdParams(sctx, a.kube, ns, a.log)
	if err != nil {
		return err
	}
	a.params, _ = settings.ResolveParams(settings.ComponentServer, cm, nil)
	p := a.params

	// Identity: a per-process token, verified by the token service as an external token,
	// gives every request the caller's Kubernetes user as principal (audit actor, Operation
	// spec.initiatedBy). Without a user name requests stay anonymous.
	verifier := &coreVerifier{}
	if who.Username != "" {
		tok, err := newCoreIdentityToken(who.Username)
		if err != nil {
			return err
		}
		a.identityToken = tok
		verifier.token = tok
		verifier.principal = auth.Principal{
			Subject: who.Username, Name: who.Username, Kind: auth.KindExternal, Issuer: "kubernetes",
			Groups: slices.Clone(who.Groups),
		}
	}
	keys, keyErr := st.SigningKeys()
	if keyErr != nil {
		// Never used to sign anything handed out: project tokens need the real key.
		if keys, err = randomKeySet(); err != nil {
			return err
		}
	}
	tokens, err := auth.NewTokenService(auth.TokenServiceConfig{
		Keys: keys, Accounts: apiserver.AccountLookup(a.settings),
		Revocations: auth.NewMemoryRevocationStore(auth.RevocationLimits{}, nil),
		Projects:    apiserver.ProjectTokenRegistry(a.projectGetter()),
		External:    verifier,
	})
	if err != nil {
		return fmt.Errorf("token service: %w", err)
	}

	// Internal clients: the repo-server and the controller tree API through Dial
	// (port-forward), authenticated with the internal token read on first use.
	repoOpts := repoServerOptionsFromParams(p, "server", "")
	repoURL, err := coreRepoURL(cfg.RepoServerURL, repoOpts)
	if err != nil {
		return err
	}
	if a.repoHostPort, a.repoPort, err = canonicalHostPort(repoURL); err != nil {
		return err
	}
	a.token = &coreTokenCache{read: cfg.InternalToken}
	if a.token.read == nil {
		a.token.read = a.readInternalToken
	}
	a.internal = a.internalTransport()
	internal := &coreBearer{next: a.internal, token: a.token.get}
	repoClientOpts := []reposerver.HTTPClientOption{reposerver.WithHTTPClient(&http.Client{Transport: internal})}
	if repoOpts.Timeout > 0 {
		repoClientOpts = append(repoClientOpts, reposerver.WithRequestTimeout(repoOpts.Timeout))
	}
	repoClient, err := reposerver.NewHTTPClient(repoURL, coreTokenPlaceholder, nil, repoClientOpts...)
	if err != nil {
		return fmt.Errorf("repo-server client: %w", err)
	}
	tree, err := a.treeClient(internal)
	if err != nil {
		return err
	}
	resolver := &coreResolver{a: a, repo: repoClient}

	appNamespaces := p.Strings("application.namespaces")
	destinations := cfg.DestinationConfig
	if destinations == nil {
		destinations = a.inClusterOnly
	}
	events := &coreAppEvents{a: a, b: applications.NewBroadcaster(0), namespaces: appNamespaces}
	recorder := &coreEventRecorder{kube: a.kube, scheme: a.scheme, namespace: ns, log: a.log}
	appSet := AppSetAPIOptions{
		EnableSCMProviders:       p.Bool("applicationsetcontroller.enable.scm.providers"),
		AllowedSCMProviders:      p.Strings("applicationsetcontroller.allowed.scm.providers"),
		SCMRootCAPath:            p.String("applicationsetcontroller.scm.root.ca.path"),
		EnableNewGitFileGlobbing: p.Bool("applicationsetcontroller.enable.new.git.file.globbing"),
	}
	genCfg := generators.Config{
		Client: a.direct, Namespace: ns,
		Repos: &generators.RepoServerService{
			Client: repoClient, Repositories: resolver.generatorResolver(),
			SubmodulesEnabled: true, NewGitFileGlobbing: appSet.EnableNewGitFileGlobbing,
		},
		Dynamic: a.dyn,
		SCM: generators.SCMConfig{
			Enabled: appSet.EnableSCMProviders, AllowedProviders: appSet.AllowedSCMProviders, RootCAPath: appSet.SCMRootCAPath,
			TokenRefStrictMode: true, Logger: a.log,
		},
		Logger: a.log,
	}
	enforcer := rbac.NewEnforcer(rbac.WithLogger(a.log))
	appSetReader := coreNamespaceScoped{WithWatch: a.direct, namespace: ns}
	services := []apiserver.Service{
		applications.New(applications.Deps{
			Config: applications.Config{
				Namespace: ns, ApplicationNamespaces: appNamespaces,
				DisableSyncWithReplace: !p.Bool("server.sync.replace.allowed"), DisableGPG: gpgDisabled(),
			},
			Client: a.direct, Settings: a.settings, Events: events,
			Repositories: resolver, RepoServer: repoClient,
			Resources: applications.ResourceDeps{
				Tree:     tree,
				Clusters: applications.ClusterConfigsFunc(destinations),
			},
			Logger: a.log,
		}),
		applicationsets.New(applicationsets.Deps{
			Config: applicationsets.Config{Namespace: ns},
			Reader: appSetReader, APIReader: appSetReader, Writer: a.direct, Watcher: appSetReader,
			Generator: applicationsets.NewGenerator(genCfg), Recorder: recorder, Logger: a.log,
		}),
		projects.New(projects.Deps{
			Config: projects.Config{Namespace: ns}, Client: a.direct, Settings: a.settings,
			Tokens: coreProjectTokens{tokens: tokens, keyErr: keyErr, namespace: ns},
			Events: a.kube.CoreV1().Events(ns), Policies: enforcer, Logger: a.log,
		}),
		clustersvc.New(clustersvc.Deps{
			Config: clustersvc.Config{Namespace: ns}, Client: a.direct, Settings: a.settings,
			// Never the stored cluster credentials: see coreClusterConnector.
			Connector: &coreClusterConnector{config: destinations, timeout: clustersvc.DefaultProbeTimeout},
			Status:    clusters.NewStatusWriter(a.dyn), Logger: a.log,
		}),
		repositories.New(repositories.Deps{
			Config: repositories.Config{
				Namespace: ns, HydratorEnabled: p.Bool("hydrator.enabled"),
				ConnectionStateTTL: p.Duration("server.connection.status.cache.expiration"),
			},
			Client: a.direct, Resolver: resolver, StatusWriter: repository.NewStatusWriter(a.dyn),
			RepoServer: repoClient, Settings: a.settings, Logger: a.log,
		}),
		notifications.New(notifications.Deps{Settings: a.settings, Logger: a.log}),
		badge.New(badge.Deps{
			Config:   badge.Config{Namespace: ns, ApplicationNamespaces: appNamespaces},
			Settings: a.settings, Reader: a.direct, Logger: a.log,
		}),
	}
	version := VersionInfo()
	version.ExtraBuildInfo = map[string]string{"mode": "core (in-process API server in the CLI; no Patrol CD server contacted)"}
	reg := prometheus.NewRegistry()
	api, err := apiserver.New(apiserver.Config{
		Namespace: ns, MetricsAddress: "-", Insecure: true, DisableAuth: true,
		Core: core.Config{
			AppsInAnyNamespaceEnabled: len(appNamespaces) > 0,
			HydratorEnabled:           p.Bool("hydrator.enabled"),
			SyncWithReplaceAllowed:    p.Bool("server.sync.replace.allowed"),
		},
	}, apiserver.Deps{
		Settings: a.settings, Tokens: tokens, Enforcer: enforcer,
		Core:          core.Deps{RepoServer: repoClient, Version: version},
		Services:      services,
		EventRecorder: recorder,
		Registry:      reg, Gatherer: reg,
		Logger: a.log,
	})
	if err != nil {
		return err
	}
	a.api = api
	a.handler = a.wrap(api.Handler())
	a.log.Debug("core mode API assembled", "namespace", ns, "user", who.Username, "repoServer", repoURL)
	return nil
}

// whoAmI returns the caller's Kubernetes identity (SelfSubjectReview); empty when the API
// is unavailable.
func (a *coreAssembly) whoAmI(ctx context.Context) authenticationv1.UserInfo {
	r, err := a.kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		a.log.Debug("SelfSubjectReview failed; requests are anonymous and Operations record no initiator", "error", err)
		return authenticationv1.UserInfo{}
	}
	return r.Status.UserInfo
}

// projectGetter reads AppProjects of the control-plane namespace.
func (a *coreAssembly) projectGetter() apiserver.ProjectGetter {
	return func(ctx context.Context, name string) (*v1alpha1.AppProject, error) {
		var p v1alpha1.AppProject
		if err := a.direct.Get(ctx, client.ObjectKey{Namespace: a.cfg.Namespace, Name: name}, &p); err != nil {
			return nil, err
		}
		return &p, nil
	}
}

// inClusterOnly serves the in-cluster destination with the caller's configuration.
func (a *coreAssembly) inClusterOnly(_ context.Context, c *v1alpha1.Cluster) (*krest.Config, error) {
	if c != nil && clusters.IsInClusterServer(c.Spec.Server) {
		return krest.CopyConfig(a.restCfg), nil
	}
	server := ""
	if c != nil {
		server = c.Spec.Server
	}
	return nil, &noDestinationConfig{server: server}
}

// noDestinationConfig is the error of a destination inClusterOnly does not serve.
type noDestinationConfig struct{ server string }

func (e *noDestinationConfig) Error() string {
	return "core mode: no kubeconfig credentials for destination cluster " + e.server
}

// NoCredentials says that nothing is wrong with the destination: core mode has no way to reach it,
// as it has none for a kubeconfig without a context for it.
func (e *noDestinationConfig) NoCredentials() {}

// treeClient routes tree API calls as patrol-server does: a fixed URL, or the replica
// holding the shard of the Application's destination cluster (status.assignedShard, else
// spec.shard, else 0; the shard Lease names the pod).
func (a *coreAssembly) treeClient(rt http.RoundTripper) (*treeapi.Client, error) {
	o := a.cfg.TreeAPI
	a.treeService = cmpOr(o.Service, treeapi.DefaultService)
	var router treeapi.Router
	if o.URL != "" {
		router = treeapi.StaticRouter{URL: o.URL}
	} else {
		scheme := "https"
		if o.TLS.Plaintext {
			scheme = "http"
		}
		router = &treeapi.ShardRouter{
			Shards:      sharding.StatusShardResolver{Clusters: coreClusterLookup{a: a}},
			Holders:     sharding.NewLeaseHolders(sharding.NewLeaseClient(a.kube, a.cfg.Namespace), 0, nil),
			StatefulSet: o.StatefulSet, Service: a.treeService, Namespace: a.cfg.Namespace, Port: o.Port, Scheme: scheme,
		}
	}
	return treeapi.NewClient(router, coreTokenPlaceholder, treeapi.WithHTTPClient(&http.Client{Transport: rt, Timeout: treeapi.DefaultClientTimeout}))
}

func cmpOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// coreRepoURL returns the repo-server URL: the override, else repo.server with the scheme
// of server.repo.server.plaintext.
func coreRepoURL(override string, o RepoServerClientOptions) (string, error) {
	raw := strings.TrimSpace(override)
	if raw == "" {
		raw = strings.TrimSpace(o.Address)
		if raw == "" {
			return "", errors.New("core mode: no repo-server address (repo.server)")
		}
		if !strings.Contains(raw, "://") {
			scheme := "https://"
			if o.TLS.Plaintext {
				scheme = "http://"
			}
			raw = scheme + raw
		}
	}
	return raw, nil
}

// canonicalHostPort returns host:port of an http(s) URL (default ports filled in).
func canonicalHostPort(raw string) (string, int, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", 0, fmt.Errorf("core mode: invalid internal address %q", raw)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return "", 0, fmt.Errorf("core mode: invalid port in internal address %q", raw)
	}
	return net.JoinHostPort(u.Hostname(), port), n, nil
}

// internalTransport carries repo-server and tree API traffic. Certificates are not
// verified (as patrol-server does by default): the internal token authenticates, and with
// port-forwarding the Kubernetes API server selects the pod over the kubeconfig's
// verified connection.
func (a *coreAssembly) internalTransport() *http.Transport {
	tr := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}, //nolint:gosec // internal token authenticates; see above
	}
	if a.cfg.Dial != nil {
		tr.DialContext = a.dialInternal
	} else {
		tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	return tr
}

// dialInternal maps the repo-server address and the tree API addresses produced by shard
// routing (<pod>.<service>.<namespace>.svc:<port>) to Dial targets; nothing else is
// reachable.
func (a *coreAssembly) dialInternal(ctx context.Context, _, addr string) (net.Conn, error) {
	target, err := a.dialTarget(addr)
	if err != nil {
		return nil, err
	}
	c, err := a.cfg.Dial(ctx, target)
	if err != nil {
		noteDiagnostic(ctx, fmt.Sprintf("cannot reach the %s: %v", target.Component, err))
		return nil, err
	}
	return c, nil
}

func (a *coreAssembly) dialTarget(addr string) (CoreDialTarget, error) {
	if strings.EqualFold(addr, a.repoHostPort) {
		return CoreDialTarget{Component: CoreComponentRepoServer, Port: a.repoPort}, nil
	}
	host, port, err := net.SplitHostPort(addr)
	if err == nil && a.cfg.TreeAPI.URL == "" {
		parts := strings.Split(host, ".")
		n, perr := strconv.Atoi(port)
		if perr == nil && len(parts) == 4 && parts[0] != "" && parts[1] == a.treeService && parts[2] == a.cfg.Namespace && parts[3] == "svc" {
			return CoreDialTarget{Component: CoreComponentController, Pod: parts[0], Port: n}, nil
		}
	}
	return CoreDialTarget{}, fmt.Errorf("core mode: no route to %s (only the repo-server and the controller of namespace %s are reachable)", addr, a.cfg.Namespace)
}

// readInternalToken reads Secret patrol-internal-token with the caller's credentials.
func (a *coreAssembly) readInternalToken(ctx context.Context) (string, error) {
	ns := a.cfg.Namespace
	sec, err := a.kube.CoreV1().Secrets(ns).Get(ctx, InternalTokenSecretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("core mode needs the internal token of the repo-server and the controller: read Secret %s/%s: %w", ns, InternalTokenSecretName, err)
	}
	tok := strings.TrimSpace(string(sec.Data[InternalTokenSecretKey]))
	if tok == "" {
		return "", fmt.Errorf("core mode: Secret %s/%s has no %q key", ns, InternalTokenSecretName, InternalTokenSecretKey)
	}
	return tok, nil
}

// coreTokenCache reads the internal token once (a failed read is retried next time).
type coreTokenCache struct {
	read func(ctx context.Context) (string, error)
	mu   sync.Mutex
	tok  string
}

func (c *coreTokenCache) get(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != "" {
		return c.tok, nil
	}
	tok, err := c.read(ctx)
	if err != nil {
		noteDiagnostic(ctx, err.Error())
		return "", err
	}
	if tok == "" {
		return "", errors.New("core mode: empty internal token")
	}
	c.tok = tok
	return tok, nil
}

// coreBearer sets the internal token on every request of the internal clients.
type coreBearer struct {
	next  http.RoundTripper
	token func(ctx context.Context) (string, error)
}

// RoundTrip implements http.RoundTripper.
func (b *coreBearer) RoundTrip(r *http.Request) (*http.Response, error) {
	tok, err := b.token(r.Context())
	if err != nil {
		if r.Body != nil {
			_ = r.Body.Close()
		}
		return nil, err
	}
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", "Bearer "+tok)
	return b.next.RoundTrip(r2)
}

// connTracker dials the Kubernetes API connections of the assembly and closes them all on
// close; connections dialed afterwards are closed at once.
type connTracker struct {
	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

func (t *connTracker) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c, err := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = c.Close()
		return nil, errors.New("core mode: the in-process API server is closed")
	}
	if t.conns == nil {
		t.conns = map[net.Conn]struct{}{}
	}
	tc := &trackedConn{Conn: c, t: t}
	t.conns[tc] = struct{}{}
	return tc, nil
}

func (t *connTracker) closeAll() {
	t.mu.Lock()
	t.closed = true
	conns := t.conns
	t.conns = nil
	t.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
}

type trackedConn struct {
	net.Conn
	t    *connTracker
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.t.mu.Lock()
		delete(c.t.conns, c)
		c.t.mu.Unlock()
	})
	return c.Conn.Close()
}
