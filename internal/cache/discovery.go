package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"

	"github.com/juliosaraiva/patrol-cd/internal/clusters"
)

// discoveryResult is one snapshot of the cluster's served APIs. It is immutable once built.
type discoveryResult struct {
	serverVersion string
	// all lists every served (group, version, resource) supporting list and watch,
	// regardless of exclusions; returned by GetAPIResources.
	all []APIResourceInfo
	// byGVK indexes every served top-level resource of every version (used for GET).
	byGVK map[schema.GroupVersionKind]APIResourceInfo
	// namespaced records the scope of every served group/kind.
	namespaced map[schema.GroupKind]bool
	// watch lists the preferred version of each group/kind to cache (list+watch, not
	// excluded), sorted by group/kind.
	watch []APIResourceInfo
	// failures lists group versions whose discovery failed.
	failures []UnavailableAPI
}

// served reports whether gk is served by the cluster. A GroupKind with an empty Kind asks
// whether any kind of the group is served.
func (d *discoveryResult) served(gk schema.GroupKind) bool {
	if gk.Kind == "" {
		for k := range d.namespaced {
			if k.Group == gk.Group {
				return true
			}
		}
		return false
	}
	_, ok := d.namespaced[gk]
	return ok
}

// failedGroups returns the groups with at least one group version whose discovery failed.
func (d *discoveryResult) failedGroups() map[string]bool {
	out := make(map[string]bool, len(d.failures))
	for _, f := range d.failures {
		out[f.GroupKind.Group] = true
	}
	return out
}

// inheritFailed copies from prev what d lacks about the group versions whose discovery
// failed, so that a transient failure does not make served kinds look unknown (IsNamespaced,
// GetAPIResources, GET resolution). It must be called before d is published; prev may be
// nil. The watch list is not inherited: callers keep existing registrations of failed
// groups instead.
func (d *discoveryResult) inheritFailed(prev *discoveryResult) {
	if prev == nil || len(d.failures) == 0 {
		return
	}
	failedGV := make(map[schema.GroupVersion]bool, len(d.failures))
	failedGroup := make(map[string]bool, len(d.failures))
	for _, f := range d.failures {
		failedGV[schema.GroupVersion{Group: f.GroupKind.Group, Version: f.Version}] = true
		failedGroup[f.GroupKind.Group] = true
	}
	for gk, ns := range prev.namespaced {
		if _, ok := d.namespaced[gk]; !ok && failedGroup[gk.Group] {
			d.namespaced[gk] = ns
		}
	}
	for gvk, info := range prev.byGVK {
		if _, ok := d.byGVK[gvk]; !ok && failedGV[gvk.GroupVersion()] {
			d.byGVK[gvk] = info
		}
	}
	have := make(map[schema.GroupVersionResource]bool, len(d.all))
	for _, a := range d.all {
		have[a.GroupVersionResource] = true
	}
	added := false
	for _, a := range prev.all {
		if !have[a.GroupVersionResource] && failedGV[a.GroupVersionResource.GroupVersion()] {
			d.all = append(d.all, a)
			added = true
		}
	}
	if added {
		slices.SortFunc(d.all, compareAPIInfo)
	}
}

// alwaysExcluded reports kinds that are never cached regardless of configuration, matching
// upstream's core exclusions: high-churn objects that no application manages.
func alwaysExcluded(gk schema.GroupKind) bool {
	switch {
	case gk.Group == "events.k8s.io", gk.Group == "metrics.k8s.io":
		return true
	case gk.Group == "" && gk.Kind == "Event":
		return true
	case gk.Group == "coordination.k8s.io" && gk.Kind == "Lease":
		return true
	}
	return false
}

func discoverAPIs(disco discovery.DiscoveryInterface, s *Settings, server string) (*discoveryResult, error) {
	info, err := disco.ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("get server version: %w", err)
	}
	groups, lists, err := disco.ServerGroupsAndResources()
	var failures []UnavailableAPI
	if err != nil {
		var gdf *discovery.ErrGroupDiscoveryFailed
		if !errors.As(err, &gdf) || len(lists) == 0 {
			return nil, fmt.Errorf("discover API resources: %w", err)
		}
		for gv, gerr := range gdf.Groups {
			failures = append(failures, UnavailableAPI{
				GroupKind: schema.GroupKind{Group: gv.Group},
				Version:   gv.Version,
				Reason:    ReasonDiscoveryFailed,
				Message:   gerr.Error(),
			})
		}
		slices.SortFunc(failures, func(a, b UnavailableAPI) int {
			return cmp.Or(cmp.Compare(a.GroupKind.Group, b.GroupKind.Group), cmp.Compare(a.Version, b.Version))
		})
	}
	d := buildDiscovery(groups, lists, s, server)
	d.serverVersion = normalizeServerVersion(info.GitVersion)
	d.failures = failures
	return d, nil
}

// servedAPIResources returns the server version and the served API resources of the cluster behind
// disco: what GetServerVersion and GetAPIResources of its cache return. As in a full sync, group
// versions whose discovery failed are left out; the third value lists them.
func servedAPIResources(disco discovery.DiscoveryInterface) (string, []APIResourceInfo, []UnavailableAPI, error) {
	d, err := discoverAPIs(disco, &Settings{}, "")
	if err != nil {
		return "", nil, nil, err
	}
	return d.serverVersion, d.all, d.failures, nil
}

// DiscoverAPIResources is what a cache of the cluster behind cfg would report, for a caller that has
// none: see servedAPIResources. The discovery client takes no context, so ctx reaches its requests
// through the transport and their rate limit through the limiter, and discoveryTimeout bounds them
// together; a wait for a Retry-After is above the transport, so only the request timeout bounds
// that. An answer ctx cut short is an error, since what it cut would pass for group versions that
// failed. A redirect is refused, as on a connection check. The connections of a transport cfg holds
// are closed with the call: clusters.ApplyTuning builds one per configuration.
func DiscoverAPIResources(ctx context.Context, cfg *rest.Config) (string, []APIResourceInfo, []UnavailableAPI, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	ctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	defer utilnet.CloseIdleConnectionsFor(cfg.Transport)
	bounded := rest.CopyConfig(cfg)
	bounded.WarningHandler = rest.NoWarnings{}
	bounded.RateLimiter = discoveryLimiter(ctx, stop, cfg)
	bounded.Wrap(clusters.RefuseRedirects)
	bounded.Wrap(func(rt http.RoundTripper) http.RoundTripper { return contextTransport{ctx: ctx, next: rt} })
	disco, err := discovery.NewDiscoveryClientForConfig(bounded)
	if err != nil {
		return "", nil, nil, fmt.Errorf("create discovery client: %w", err)
	}
	version, resources, unavailable, err := servedAPIResources(disco)
	if ctx.Err() != nil {
		return "", nil, nil, fmt.Errorf("discover API resources: %w", context.Cause(ctx))
	}
	if err != nil {
		return "", nil, nil, err
	}
	return version, resources, unavailable, nil
}

// discoveryLimiter is the rate limiter of cfg, or the one newClients gives a configuration with
// none, waiting under ctx. It is nil where client-go has none, for a negative QPS.
func discoveryLimiter(ctx context.Context, stop context.CancelCauseFunc, cfg *rest.Config) flowcontrol.RateLimiter {
	limiter := cfg.RateLimiter
	if limiter == nil {
		qps, burst := cfg.QPS, cfg.Burst
		if qps == 0 {
			qps, burst = defaultQPS, defaultBurst
		}
		if qps < 0 {
			return nil
		}
		limiter = flowcontrol.NewTokenBucketRateLimiter(qps, cmp.Or(burst, defaultBurst))
	}
	return contextLimiter{RateLimiter: limiter, ctx: ctx, stop: stop}
}

// contextLimiter waits under ctx: the discovery client's requests carry context.TODO(), and the
// limiter comes before the transport. A wait that cannot end in time stops the call, because the
// group version it holds up would pass for one that failed.
type contextLimiter struct {
	flowcontrol.RateLimiter
	ctx  context.Context
	stop context.CancelCauseFunc
}

// Wait implements flowcontrol.RateLimiter.
func (l contextLimiter) Wait(context.Context) error {
	err := l.RateLimiter.Wait(l.ctx)
	if err != nil {
		l.stop(err)
	}
	return err
}

// contextTransport sends every request under ctx.
type contextTransport struct {
	ctx  context.Context
	next http.RoundTripper
}

// RoundTrip implements http.RoundTripper.
func (t contextTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.next.RoundTrip(r.WithContext(t.ctx))
}

// WrappedRoundTripper exposes the inner transport to client-go utilities.
func (t contextTransport) WrappedRoundTripper() http.RoundTripper { return t.next }

// buildDiscovery derives the cache's API view from discovery documents. For each group the
// preferred version of every resource is chosen (falling back to the first version serving
// it), mirroring client-go's ServerPreferredResources.
func buildDiscovery(groups []*metav1.APIGroup, lists []*metav1.APIResourceList, s *Settings, server string) *discoveryResult {
	d := &discoveryResult{
		byGVK:      map[schema.GroupVersionKind]APIResourceInfo{},
		namespaced: map[schema.GroupKind]bool{},
	}
	byGV := make(map[string]*metav1.APIResourceList, len(lists))
	for _, l := range lists {
		if l == nil {
			continue
		}
		byGV[l.GroupVersion] = l
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range l.APIResources {
			if !topLevel(&r, gv) {
				continue
			}
			info := apiInfo(gv, r)
			if _, ok := d.byGVK[info.GroupVersionKind()]; !ok {
				d.byGVK[info.GroupVersionKind()] = info
			}
			d.namespaced[info.GroupKind] = r.Namespaced
			if listWatchable(&r) {
				d.all = append(d.all, info)
			}
		}
	}
	slices.SortFunc(d.all, compareAPIInfo)

	type choice struct {
		info      APIResourceInfo
		preferred bool
	}
	chosen := map[schema.GroupResource]choice{}
	for _, g := range groups {
		if g == nil {
			continue
		}
		for _, v := range g.Versions {
			l := byGV[v.GroupVersion]
			if l == nil {
				continue
			}
			gv, err := schema.ParseGroupVersion(v.GroupVersion)
			if err != nil {
				continue
			}
			isPreferred := v.Version == g.PreferredVersion.Version
			for _, r := range l.APIResources {
				if !topLevel(&r, gv) {
					continue
				}
				gr := schema.GroupResource{Group: gv.Group, Resource: r.Name}
				if c, ok := chosen[gr]; ok && (c.preferred || !isPreferred) {
					continue
				}
				chosen[gr] = choice{info: apiInfo(gv, r), preferred: isPreferred}
			}
		}
	}
	byGK := map[schema.GroupKind]APIResourceInfo{}
	for _, c := range chosen {
		gk := c.info.GroupKind
		if !listWatchable(&c.info.Meta) || alwaysExcluded(gk) || s.isExcluded(gk.Group, gk.Kind, server) {
			continue
		}
		// Two resources of one group serving the same kind: keep the lexically first
		// resource name so the choice is deterministic.
		if prev, ok := byGK[c.info.GroupKind]; ok && prev.GroupVersionResource.Resource < c.info.GroupVersionResource.Resource {
			continue
		}
		byGK[c.info.GroupKind] = c.info
	}
	for _, info := range byGK {
		d.watch = append(d.watch, info)
	}
	slices.SortFunc(d.watch, compareAPIInfo)
	return d
}

func topLevel(r *metav1.APIResource, gv schema.GroupVersion) bool {
	if strings.Contains(r.Name, "/") || r.Kind == "" {
		return false
	}
	return r.Group == "" || r.Group == gv.Group
}

func apiInfo(gv schema.GroupVersion, r metav1.APIResource) APIResourceInfo {
	r.Group, r.Version = gv.Group, gv.Version
	return APIResourceInfo{
		GroupKind:            schema.GroupKind{Group: gv.Group, Kind: r.Kind},
		GroupVersionResource: gv.WithResource(r.Name),
		Meta:                 r,
	}
}

func listWatchable(r *metav1.APIResource) bool {
	return hasVerb(r.Verbs, "list") && hasVerb(r.Verbs, "watch")
}

func hasVerb(verbs []string, verb string) bool {
	for _, v := range verbs {
		if strings.EqualFold(v, verb) {
			return true
		}
	}
	return false
}

func compareAPIInfo(a, b APIResourceInfo) int {
	return cmp.Or(
		cmp.Compare(a.GroupKind.Group, b.GroupKind.Group),
		cmp.Compare(a.GroupKind.Kind, b.GroupKind.Kind),
		cmp.Compare(a.GroupVersionResource.Version, b.GroupVersionResource.Version),
		cmp.Compare(a.GroupVersionResource.Resource, b.GroupVersionResource.Resource),
	)
}

// APIVersionStrings lists "group/version" and "group/version/Kind" of every served resource,
// sorted (upstream APIResourcesToStrings with kinds).
func APIVersionStrings(resources []APIResourceInfo) []string {
	set := map[string]bool{}
	for _, r := range resources {
		gv := r.GroupVersionResource.GroupVersion().String()
		set[gv] = true
		set[gv+"/"+r.GroupKind.Kind] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// normalizeServerVersion turns a git version such as "v1.30.2-eks-1234" into "v1.30.2",
// the form Helm expects for capability checks.
func normalizeServerVersion(gitVersion string) string {
	v, err := version.ParseGeneric(gitVersion)
	if err != nil {
		return gitVersion
	}
	return "v" + v.String()
}
