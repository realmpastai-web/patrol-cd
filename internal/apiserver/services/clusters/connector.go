package clusters

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	clusterlib "github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// ErrExecNotAllowed is returned by the default Connector, when ConnectorOptions.AllowExec is
// false, for clusters of another server than the in-cluster one whose configuration sets
// execProviderConfig or awsAuthConfig: patrol-server then neither runs user-supplied
// commands nor mints tokens with its own cloud identity (AWS, and the in-process GCP and
// Azure forms of the k8s-auth helper) for a server URL chosen by the caller. The connection
// is checked by the application controller instead.
var ErrExecNotAllowed = errors.New("exec credential plugins and cloud identity tokens are not used by the API server")

// DefaultProbeTimeout bounds a connection check.
const DefaultProbeTimeout = 15 * time.Second

// ConnectorOptions configures NewConnector.
type ConnectorOptions struct {
	// Options configures the connection (tuning, in-cluster configuration, cloud
	// providers); see internal/clusters.
	Options clusterlib.Options
	// AllowExec lets probes run exec credential plugins and obtain tokens for awsAuthConfig
	// and k8s-auth exec configurations with patrol-server's own cloud identity (upstream
	// behavior). Off by default: anyone allowed to create clusters could otherwise run
	// commands in patrol-server, or have its AWS, GCP or Azure tokens sent to a server they
	// control.
	AllowExec bool
	// AllowLinkLocal lets connections reach link-local and cloud metadata addresses.
	AllowLinkLocal bool
	// Timeout bounds each probe (default DefaultProbeTimeout).
	Timeout time.Duration
}

// NewConnector returns the default Connector: internal/clusters.RawRestConfig with a
// dialer that refuses link-local and cloud metadata addresses (unless allowed), redirects
// refused, and exec plugins and ambient cloud identity refused unless allowed.
func NewConnector(o ConnectorOptions) Connector {
	if o.Timeout <= 0 {
		o.Timeout = DefaultProbeTimeout
	}
	if !o.AllowExec {
		// Never build in-process token sources (they would authenticate with this
		// process's identity): awsAuthConfig and k8s-auth exec configurations then take
		// their exec form, which config refuses. usesAmbientAuth refuses them earlier.
		o.Options.AWSAuthExec = true
		o.Options.DisableNativeK8sAuth = true
	}
	return &connector{o: o}
}

type connector struct {
	o ConnectorOptions
}

func (c *connector) config(ctx context.Context, cl *v1alpha1.Cluster, data map[string][]byte) (*rest.Config, error) {
	if !c.o.AllowExec && usesAmbientAuth(&cl.Spec) {
		return nil, ErrExecNotAllowed
	}
	cfg, err := clusterlib.RawRestConfig(ctx, cl, data, c.o.Options)
	if err != nil {
		return nil, err
	}
	if cfg.ExecProvider != nil && !c.o.AllowExec {
		return nil, ErrExecNotAllowed
	}
	cfg = rest.CopyConfig(cfg)
	if !c.o.AllowLinkLocal {
		d := &net.Dialer{Timeout: c.o.Timeout, KeepAlive: 30 * time.Second, Control: guardControl}
		cfg.Dial = d.DialContext
	}
	cfg.Wrap(clusterlib.RefuseRedirects)
	cfg.Timeout = c.o.Timeout
	return cfg, nil
}

func (c *connector) Probe(ctx context.Context, cl *v1alpha1.Cluster, data map[string][]byte) (string, error) {
	cfg, err := c.config(ctx, cl, data)
	if err != nil {
		return "", err
	}
	pctx, cancel := context.WithTimeout(ctx, c.o.Timeout)
	defer cancel()
	return clusterlib.Probe(pctx, cfg)
}

func (c *connector) Kubernetes(ctx context.Context, cl *v1alpha1.Cluster, data map[string][]byte) (kubernetes.Interface, error) {
	cfg, err := c.config(ctx, cl, data)
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// metadataAddrs are cloud instance metadata endpoints outside the link-local ranges.
var metadataAddrs = []netip.Addr{
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IMDS over IPv6
	netip.MustParseAddr("100.100.100.200"), // Alibaba Cloud metadata
}

// metadataHosts are host names of cloud metadata services.
var metadataHosts = map[string]bool{
	"metadata.google.internal":   true,
	"metadata.goog":              true,
	"metadata":                   true,
	"instance-data":              true,
	"instance-data.ec2.internal": true,
}

// blockedAddr reports whether connecting to ip is refused by the SSRF guard: unspecified,
// link-local (cloud metadata lives at 169.254.169.254 and fe80::), multicast and known
// metadata addresses. Loopback and private addresses stay allowed: destination clusters
// commonly live there.
func blockedAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid(), ip.IsUnspecified(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return true
	}
	for _, m := range metadataAddrs {
		if ip == m {
			return true
		}
	}
	return false
}

// blockedHost reports whether a URL host (name or literal address, without port) is
// refused by the SSRF guard.
func blockedHost(host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if ip, err := netip.ParseAddr(h); err == nil {
		return blockedAddr(ip)
	}
	return metadataHosts[h]
}

// guardControl is the net.Dialer Control hook: it runs after name resolution, so names that
// resolve to refused addresses are caught too.
func guardControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", address, err)
	}
	if blockedAddr(ip) {
		return fmt.Errorf("connections to %s are not allowed (link-local, multicast or metadata address)", ip)
	}
	return nil
}

// checkURLHost validates the host of a server or proxy URL against the SSRF guard.
func checkURLHost(u *url.URL, allowLinkLocal bool) error {
	if allowLinkLocal {
		return nil
	}
	if blockedHost(u.Hostname()) {
		return fmt.Errorf("host %q is a link-local, multicast or metadata address", u.Hostname())
	}
	return nil
}
