package compare

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/health"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

// Info item names (upstream common.PodRequestsCPU / PodRequestsMEM and info.go literals).
const (
	InfoStatusReason  = "Status Reason"
	InfoNode          = "Node"
	InfoContainers    = "Containers"
	InfoRestartCount  = "Restart Count"
	InfoRevision      = "Revision"
	InfoRequestsCPU   = "cpu"
	InfoRequestsMem   = "memory"
	InfoManagedByURL  = "managed-by-url"
	gatewayAPIGroup   = "gateway.networking.k8s.io"
	istioNetworkGroup = "networking.istio.io"
)

// PodInfo is kept for every Pod to compute node usage (tree hosts).
type PodInfo struct {
	NodeName         string
	ResourceRequests corev1.ResourceList
	Phase            corev1.PodPhase
}

// NodeInfo is kept for every Node (tree hosts).
type NodeInfo struct {
	Name       string
	Capacity   corev1.ResourceList
	SystemInfo corev1.NodeSystemInfo
	Labels     map[string]string
}

// ResourceInfo is the value stored in cache.Resource.Info by the populator of CacheSettings
// (upstream controller/cache ResourceInfo). Values are immutable once stored.
type ResourceInfo struct {
	// Info are the info items shown for the resource.
	Info []InfoItem
	// AppName is the instance name of the Application tracking the resource; set for root
	// resources (no owner references) only.
	AppName string
	// Images are the container images of a Pod.
	Images []string
	// Health is the resource's own health (nil for kinds without health or on error).
	Health *health.HealthStatus
	// NetworkingInfo is set for networking kinds, Pods and resources with link annotations.
	NetworkingInfo *NetworkingInfo
	// PodInfo is set for Pods, NodeInfo for Nodes.
	PodInfo  *PodInfo
	NodeInfo *NodeInfo
	// ManifestHash is the hash of the manifest without the fields that ignoreResourceUpdates
	// rules ignore; empty when not computed.
	ManifestHash string
}

// InfoOf returns the ResourceInfo of a cached resource, or an empty one.
func InfoOf(r *cache.Resource) *ResourceInfo {
	if r != nil {
		if info, ok := r.Info.(*ResourceInfo); ok && info != nil {
			return info
		}
	}
	return &ResourceInfo{}
}

// SkipResourceUpdate reports whether a resource update changed neither the health status nor
// the hashed manifest, so it need not trigger a refresh (upstream skipResourceUpdate, used
// when resource.ignoreResourceUpdatesEnabled is true).
func SkipResourceUpdate(oldInfo, newInfo *ResourceInfo) bool {
	if oldInfo == nil || newInfo == nil {
		return false
	}
	sameHealth := (oldInfo.Health == nil && newInfo.Health == nil) ||
		(oldInfo.Health != nil && newInfo.Health != nil && oldInfo.Health.Status == newInfo.Health.Status)
	sameManifest := oldInfo.ManifestHash != "" && newInfo.ManifestHash != "" && oldInfo.ManifestHash == newInfo.ManifestHash
	return sameHealth && sameManifest
}

// AppOf returns the instance name of the Application a cached resource belongs to: its own
// tracking when it is a tracked root, else the first owner chain within its namespace that
// leads to one (upstream getApp). Cycles stop the walk.
func AppOf(r *cache.Resource, namespaceResources map[ResourceKey]*cache.Resource) string {
	name, _ := appRecursive(r, namespaceResources, map[ResourceKey]bool{})
	return name
}

// appRecursive searches r and its owners, each object once. visited[key] is true while key is
// on the current chain, so reaching it again is a cycle, and false once its owners led nowhere,
// so a chain that shares that owner skips it. An owner that led nowhere has no path back to any
// chain: the search that cleared it would have met the cycle. The start object can be older than
// the copy in ns, so a cycle back to it answers with that copy's Application.
func appRecursive(r *cache.Resource, ns map[ResourceKey]*cache.Resource, visited map[ResourceKey]bool) (string, bool) {
	key := r.ResourceKey()
	if onChain, ok := visited[key]; ok {
		return InfoOf(r).AppName, !onChain
	}
	visited[key] = true
	if name := InfoOf(r).AppName; name != "" {
		return name, true
	}
	for _, ref := range r.OwnerRefs {
		group := groupOfAPIVersion(ref.APIVersion)
		parent, ok := ns[ResourceKey{Group: group, Kind: ref.Kind, Namespace: r.Ref.Namespace, Name: ref.Name}]
		if !ok {
			continue
		}
		if app, ok := appRecursive(parent, ns, visited); app != "" || !ok {
			return app, ok
		}
	}
	visited[key] = false
	return "", true
}

// groupOfAPIVersion returns the group of "group/version" ("" for "v1" or unparsable).
func groupOfAPIVersion(apiVersion string) string {
	if g, _, ok := strings.Cut(apiVersion, "/"); ok {
		return g
	}
	return ""
}

// InfoConfig configures resource info population.
type InfoConfig struct {
	// Tracker, Method and InstallationID attribute root resources to Applications.
	Tracker        *tracking.ResourceTracker
	Method         tracking.Method
	InstallationID string
	// Health supplies Lua health customizations (nil: built-in checks only).
	Health health.Overrides
	// CustomLabels are resource.customLabels (info items).
	CustomLabels []string
	// UpdateNormalizer, when set, enables manifest hashing for ignoreResourceUpdates.
	UpdateNormalizer diff.Normalizer
}

// PopulateResourceInfo returns the cache.Settings.PopulateResourceInfo function computing
// ResourceInfo (upstream SetPopulateResourceInfoHandler): info items, networking info,
// health, Application attribution of root resources and the update hash. The full manifest
// is retained for tracked roots and CRDs.
func PopulateResourceInfo(cfg InfoConfig) func(un *unstructured.Unstructured, isRoot bool) (any, bool) {
	return func(un *unstructured.Unstructured, isRoot bool) (any, bool) {
		res := &ResourceInfo{}
		populateNodeInfo(un, res, cfg.CustomLabels)
		if h, err := health.Assess(un, cfg.Health); err == nil {
			res.Health = h
		}
		appName := ""
		if cfg.Tracker != nil {
			appName = cfg.Tracker.GetAppName(un, cfg.Method, cfg.InstallationID)
		}
		if isRoot && appName != "" {
			res.AppName = appName
		}
		gvk := un.GroupVersionKind()
		if cfg.UpdateNormalizer != nil && shouldHashManifest(appName, gvk.Group, gvk.Kind, un) {
			if hash, err := diff.ResourceUpdateHash(context.Background(), un, cfg.UpdateNormalizer); err == nil {
				res.ManifestHash = hash
			}
		}
		return res, res.AppName != "" || (gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "CustomResourceDefinition")
	}
}

// shouldHashManifest mirrors upstream: tracked resources and Applications are hashed;
// untracked ones only with ignore-resource-updates set to a true value.
func shouldHashManifest(appName, group, kind string, un *unstructured.Unstructured) bool {
	if appName != "" || (group == v1alpha1.GroupName && kind == "Application") {
		return true
	}
	v, ok := un.GetAnnotations()[v1alpha1.AnnotationKeyIgnoreResourceUpdates]
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// populateNodeInfo fills the info items and networking info of un (upstream populateNodeInfo).
func populateNodeInfo(un *unstructured.Unstructured, res *ResourceInfo, customLabels []string) {
	if rev := revisionOf(un); rev > 0 {
		res.Info = append(res.Info, InfoItem{Name: InfoRevision, Value: fmt.Sprintf("Rev:%d", rev)})
	}
	if len(customLabels) > 0 {
		labels := un.GetLabels()
		for _, l := range customLabels {
			if v, ok := labels[l]; ok {
				res.Info = append(res.Info, InfoItem{Name: l, Value: v})
			}
		}
	}
	annotations := un.GetAnnotations()
	for _, k := range slices.Sorted(maps.Keys(annotations)) {
		if !strings.HasPrefix(k, v1alpha1.AnnotationKeyLinkPrefix) {
			continue
		}
		v := annotations[k]
		urlPart := v
		if _, after, ok := strings.Cut(v, "|"); ok {
			urlPart = after
		}
		if settings.ValidateExternalURL(urlPart) != nil {
			continue
		}
		if res.NetworkingInfo == nil {
			res.NetworkingInfo = &NetworkingInfo{}
		}
		res.NetworkingInfo.ExternalURLs = append(res.NetworkingInfo.ExternalURLs, v)
	}

	gvk := un.GroupVersionKind()
	switch gvk.Group {
	case "":
		switch gvk.Kind {
		case "Pod":
			populatePodInfo(un, res)
		case "Service":
			populateServiceInfo(un, res)
		case "Node":
			populateHostNodeInfo(un, res)
		}
	case "extensions", "networking.k8s.io":
		if gvk.Kind == "Ingress" {
			populateIngressInfo(un, res)
		}
	case istioNetworkGroup:
		switch gvk.Kind {
		case "VirtualService":
			populateIstioVirtualServiceInfo(un, res)
		case "ServiceEntry":
			populateIstioServiceEntryInfo(un, res)
		}
	case gatewayAPIGroup:
		switch gvk.Kind {
		case "HTTPRoute", "GRPCRoute", "TCPRoute", "TLSRoute", "UDPRoute":
			populateGatewayAPIRouteInfo(un, res)
		case "Gateway":
			populateGatewayInfo(un, res)
		}
	case v1alpha1.GroupName:
		if gvk.Kind == "Application" {
			if url, ok := annotations[v1alpha1.AnnotationKeyManagedByURL]; ok {
				res.Info = append(res.Info, InfoItem{Name: InfoManagedByURL, Value: url})
			}
		}
	}
}

// revisionOf returns the deployment/rollout revision annotation (first present) or the
// top-level integer revision field (ControllerRevision); 0 when absent or invalid.
func revisionOf(un *unstructured.Unstructured) int64 {
	annotations := un.GetAnnotations()
	for _, name := range []string{"deployment.kubernetes.io/revision", "rollout.argoproj.io/revision"} {
		if text, ok := annotations[name]; ok {
			rev, _ := strconv.ParseInt(text, 10, 64)
			return rev
		}
	}
	if rev, ok := un.Object["revision"].(int64); ok {
		return rev
	}
	return 0
}

func existingURLs(res *ResourceInfo) []string {
	if res.NetworkingInfo == nil {
		return nil
	}
	return res.NetworkingInfo.ExternalURLs
}

func ignoreDefaultLinks(un *unstructured.Unstructured) bool {
	v, ok := un.GetAnnotations()[v1alpha1.AnnotationKeyIgnoreDefaultLinks]
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// loadBalancerIngress returns status.loadBalancer.ingress (hostname preferred over IP).
func loadBalancerIngress(un *unstructured.Unstructured) []LoadBalancerIngress {
	items, ok, err := unstructured.NestedSlice(un.Object, "status", "loadBalancer", "ingress")
	if !ok || err != nil {
		return nil
	}
	out := make([]LoadBalancerIngress, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if h := m["hostname"]; h != nil {
			out = append(out, LoadBalancerIngress{Hostname: fmt.Sprint(h)})
		} else if ip := m["ip"]; ip != nil {
			out = append(out, LoadBalancerIngress{IP: fmt.Sprint(ip)})
		}
	}
	return out
}

func populateServiceInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	targetLabels, _, _ := unstructured.NestedStringMap(un.Object, "spec", "selector")
	ingress := []LoadBalancerIngress{}
	if t, ok, err := unstructured.NestedString(un.Object, "spec", "type"); ok && err == nil && t == string(corev1.ServiceTypeLoadBalancer) {
		ingress = loadBalancerIngress(un)
	}
	res.NetworkingInfo = &NetworkingInfo{TargetLabels: targetLabels, Ingress: ingress, ExternalURLs: existingURLs(res)}
}

// ingressServiceName returns the backend Service name for the Ingress API version.
func ingressServiceName(backend map[string]any, group, version string) (string, bool) {
	switch {
	case group == "extensions", group == "networking.k8s.io" && version == "v1beta1":
		return fmt.Sprint(backend["serviceName"]), true
	case group == "networking.k8s.io" && version == "v1":
		if svc, ok, err := unstructured.NestedMap(backend, "service"); ok && err == nil {
			return fmt.Sprint(svc["name"]), true
		}
	}
	return "", false
}

func populateIngressInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	ingress := loadBalancerIngress(un)
	gvk := un.GroupVersionKind()
	targets := map[ResourceRef]bool{}
	addTarget := func(backend map[string]any) {
		if name, ok := ingressServiceName(backend, gvk.Group, gvk.Version); ok {
			targets[ResourceRef{Kind: "Service", Namespace: un.GetNamespace(), Name: name}] = true
		}
	}
	if backend, ok, err := unstructured.NestedMap(un.Object, "spec", "backend"); ok && err == nil {
		addTarget(backend)
	}
	urls := map[string]bool{}
	rules, ok, err := unstructured.NestedSlice(un.Object, "spec", "rules")
	if ok && err == nil {
		tls, _, _ := unstructured.NestedSlice(un.Object, "spec", "tls")
		for _, r := range rules {
			rule, ok := r.(map[string]any)
			if !ok {
				continue
			}
			host := rule["host"]
			if host == nil || host == "" {
				for _, in := range ingress {
					if h := firstNonEmpty(in.Hostname, in.IP); h != "" {
						host = h
						break
					}
				}
			}
			paths, ok, err := unstructured.NestedSlice(rule, "http", "paths")
			if !ok || err != nil {
				continue
			}
			for _, p := range paths {
				path, ok := p.(map[string]any)
				if !ok {
					continue
				}
				if backend, ok, err := unstructured.NestedMap(path, "backend"); ok && err == nil {
					addTarget(backend)
				}
				if host == nil || host == "" {
					continue
				}
				scheme := "http"
				for _, t := range tls {
					line, ok := t.(map[string]any)
					if !ok {
						continue
					}
					if line["secretName"] != nil {
						scheme = "https"
					}
					if line["host"] == host {
						scheme = "https"
					}
					// Upstream type-asserts tls[].hosts (a list) as a map, so host lists never
					// switch the scheme; reproduced for parity (DIFF-091).
				}
				u := fmt.Sprintf("%s://%v", scheme, host)
				if sub, ok, err := unstructured.NestedString(path, "path"); ok && err == nil {
					u += strings.TrimSuffix(sub, "*")
				}
				urls[u] = true
			}
		}
	}
	all := existingURLs(res)
	if !ignoreDefaultLinks(un) {
		all = append(all, slices.Sorted(maps.Keys(urls))...)
	}
	res.NetworkingInfo = &NetworkingInfo{TargetRefs: sortedRefs(targets), Ingress: ingress, ExternalURLs: all}
}

func populateIstioVirtualServiceInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	targets := map[ResourceRef]bool{}
	rules, ok, err := unstructured.NestedSlice(un.Object, "spec", "http")
	if ok && err == nil {
		for _, r := range rules {
			rule, ok := r.(map[string]any)
			if !ok {
				continue
			}
			routes, ok, err := unstructured.NestedSlice(rule, "route")
			if !ok || err != nil {
				continue
			}
			for _, rt := range routes {
				route, ok := rt.(map[string]any)
				if !ok {
					continue
				}
				host, ok, err := unstructured.NestedString(route, "destination", "host")
				if !ok || err != nil {
					continue
				}
				parts := strings.Split(host, ".")
				ns := un.GetNamespace()
				if len(parts) >= 2 {
					ns = parts[1]
				}
				targets[ResourceRef{Kind: "Service", Name: parts[0], Namespace: ns}] = true
			}
		}
	}
	res.NetworkingInfo = &NetworkingInfo{TargetRefs: sortedRefs(targets), ExternalURLs: existingURLs(res)}
}

// populateIstioServiceEntryInfo replaces the networking info wholesale when a workload
// selector is present (upstream quirk: link URLs are dropped then, DIFF-094).
func populateIstioServiceEntryInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	labels, ok, err := unstructured.NestedStringMap(un.Object, "spec", "workloadSelector", "labels")
	if err != nil || !ok {
		return
	}
	res.NetworkingInfo = &NetworkingInfo{TargetLabels: labels, TargetRefs: []ResourceRef{{Kind: "Pod"}}}
}

func gatewayAddresses(un *unstructured.Unstructured) []LoadBalancerIngress {
	addrs, ok, err := unstructured.NestedSlice(un.Object, "status", "addresses")
	if !ok || err != nil {
		return nil
	}
	out := make([]LoadBalancerIngress, 0, len(addrs))
	for _, a := range addrs {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		t, _, _ := unstructured.NestedString(m, "type")
		v, _, _ := unstructured.NestedString(m, "value")
		if v == "" {
			continue
		}
		switch t {
		case "Hostname":
			out = append(out, LoadBalancerIngress{Hostname: v})
		case "IPAddress", "":
			out = append(out, LoadBalancerIngress{IP: v})
		}
	}
	return out
}

func gatewayURLs(un *unstructured.Unstructured, addresses []LoadBalancerIngress) []string {
	listeners, ok, err := unstructured.NestedSlice(un.Object, "spec", "listeners")
	if !ok || err != nil {
		return nil
	}
	urls := map[string]bool{}
	for _, l := range listeners {
		m, ok := l.(map[string]any)
		if !ok {
			continue
		}
		hostname, _, _ := unstructured.NestedString(m, "hostname")
		protocol, _, _ := unstructured.NestedString(m, "protocol")
		var port int64
		switch v := m["port"].(type) {
		case int64:
			port = v
		case float64:
			port = int64(v)
		case int:
			port = int64(v)
		}
		scheme := "http"
		if protocol == "HTTPS" || protocol == "TLS" {
			scheme = "https"
		}
		var hosts []string
		if hostname != "" && hostname != "*" {
			hosts = append(hosts, hostname)
		} else {
			for _, a := range addresses {
				if h := firstNonEmpty(a.Hostname, a.IP); h != "" {
					hosts = append(hosts, h)
				}
			}
		}
		for _, h := range hosts {
			u := scheme + "://" + h
			if (scheme == "http" && port != 80) || (scheme == "https" && port != 443) {
				u = fmt.Sprintf("%s:%d", u, port)
			}
			if settings.ValidateExternalURL(u) != nil {
				continue
			}
			urls[u] = true
		}
	}
	return slices.Sorted(maps.Keys(urls))
}

func populateGatewayInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	ingress := gatewayAddresses(un)
	urls := existingURLs(res)
	if !ignoreDefaultLinks(un) {
		urls = append(urls, gatewayURLs(un, ingress)...)
	}
	res.NetworkingInfo = &NetworkingInfo{Ingress: ingress, ExternalURLs: urls}
}

func populateGatewayAPIRouteInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	targets := map[ResourceRef]bool{}
	str := func(m map[string]any, field, def string) string {
		if v, _, _ := unstructured.NestedString(m, field); v != "" {
			return v
		}
		return def
	}
	if parents, ok, err := unstructured.NestedSlice(un.Object, "spec", "parentRefs"); ok && err == nil {
		for _, p := range parents {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			name := str(m, "name", "")
			if name == "" {
				continue
			}
			targets[ResourceRef{
				Group:     str(m, "group", gatewayAPIGroup),
				Kind:      str(m, "kind", "Gateway"),
				Namespace: str(m, "namespace", un.GetNamespace()),
				Name:      name,
			}] = true
		}
	}
	if rules, ok, err := unstructured.NestedSlice(un.Object, "spec", "rules"); ok && err == nil {
		for _, r := range rules {
			rule, ok := r.(map[string]any)
			if !ok {
				continue
			}
			backends, ok, err := unstructured.NestedSlice(rule, "backendRefs")
			if !ok || err != nil {
				continue
			}
			for _, b := range backends {
				m, ok := b.(map[string]any)
				if !ok {
					continue
				}
				name := str(m, "name", "")
				if name == "" {
					continue
				}
				targets[ResourceRef{
					Group:     str(m, "group", ""),
					Kind:      str(m, "kind", "Service"),
					Namespace: str(m, "namespace", un.GetNamespace()),
					Name:      name,
				}] = true
			}
		}
	}
	res.NetworkingInfo = &NetworkingInfo{TargetRefs: sortedRefs(targets), ExternalURLs: existingURLs(res)}
}

func populateHostNodeInfo(un *unstructured.Unstructured, res *ResourceInfo) {
	var node corev1.Node
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(un.Object, &node); err != nil {
		return
	}
	res.NodeInfo = &NodeInfo{Name: node.Name, Capacity: node.Status.Capacity, SystemInfo: node.Status.NodeInfo, Labels: node.Labels}
}

func sortedRefs(set map[ResourceRef]bool) []ResourceRef {
	out := make([]ResourceRef, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b ResourceRef) int {
		return strings.Compare(a.FullName(), b.FullName())
	})
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
