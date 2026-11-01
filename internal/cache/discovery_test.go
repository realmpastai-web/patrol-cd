package cache

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sversion "k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
)

type excludeFilter map[schema.GroupKind]bool

func (f excludeFilter) IsExcluded(group, kind, _ string) bool {
	return f[schema.GroupKind{Group: group, Kind: kind}]
}

func TestBuildDiscovery(t *testing.T) {
	t.Parallel()
	lw := []string{"get", "list", "watch"}
	groups := []*metav1.APIGroup{
		{Name: "", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "v1", Version: "v1"}},
		{Name: "autoscaling", Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "autoscaling/v1", Version: "v1"}, {GroupVersion: "autoscaling/v2", Version: "v2"},
		}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "autoscaling/v2", Version: "v2"}},
		{Name: "coordination.k8s.io", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "coordination.k8s.io/v1", Version: "v1"}},
			PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "coordination.k8s.io/v1", Version: "v1"}},
		{Name: "events.k8s.io", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "events.k8s.io/v1", Version: "v1"}},
			PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "events.k8s.io/v1", Version: "v1"}},
		{Name: "example.com", Versions: []metav1.GroupVersionForDiscovery{
			{GroupVersion: "example.com/v2", Version: "v2"}, {GroupVersion: "example.com/v1", Version: "v1"},
		}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "example.com/v2", Version: "v2"}},
	}
	lists := []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: lw},
			{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: lw},
			{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: []string{"get"}},
			{Name: "namespaces", Kind: "Namespace", Verbs: lw},
			{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: []string{"create"}},
			{Name: "secrets", Kind: "Secret", Namespaced: true, Verbs: lw},
			{Name: "events", Kind: "Event", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "coordination.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "leases", Kind: "Lease", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "events.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "events", Kind: "Event", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "autoscaling/v1", APIResources: []metav1.APIResource{
			{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "autoscaling/v2", APIResources: []metav1.APIResource{
			{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "example.com/v2", APIResources: []metav1.APIResource{
			{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: lw},
		}},
		{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: lw},
			{Name: "gadgets", Kind: "Gadget", Verbs: lw}, // only served in a non-preferred version
		}},
	}
	s := Settings{ResourceFilter: excludeFilter{{Kind: "Secret"}: true}}
	d := buildDiscovery(groups, lists, &s, "https://cluster")

	var watched []string
	for _, a := range d.watch {
		watched = append(watched, a.GroupVersionResource.String())
	}
	assert.Equal(t, []string{
		"/v1, Resource=configmaps",
		"/v1, Resource=namespaces",
		"/v1, Resource=pods",
		"autoscaling/v2, Resource=horizontalpodautoscalers",
		"example.com/v1, Resource=gadgets",
		"example.com/v2, Resource=widgets",
	}, watched, "preferred versions, list+watch only, exclusions (configured and always-on) applied, subresources skipped")

	assert.Len(t, d.all, 12, "GetAPIResources lists every served version with list+watch, including excluded kinds")
	assert.True(t, d.served(schema.GroupKind{Group: "events.k8s.io"}), "group-level query")
	assert.False(t, d.served(schema.GroupKind{Group: "absent.example.com"}))
	assert.True(t, d.namespaced[schema.GroupKind{Kind: "Secret"}], "excluded kinds still have known scope")
	assert.False(t, d.namespaced[schema.GroupKind{Group: "example.com", Kind: "Gadget"}])
	assert.True(t, d.served(schema.GroupKind{Kind: "Binding"}))
	assert.False(t, d.served(schema.GroupKind{Group: "nope", Kind: "X"}))
	_, ok := d.byGVK[schema.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "HorizontalPodAutoscaler"}]
	assert.True(t, ok, "non-preferred versions are resolvable for GET")
	assert.Equal(t, "autoscaling", d.byGVK[schema.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "HorizontalPodAutoscaler"}].Meta.Group)
}

func TestAPIVersionStrings(t *testing.T) {
	t.Parallel()
	info := func(group, version, resource, kind string) APIResourceInfo {
		return APIResourceInfo{
			GroupKind:            schema.GroupKind{Group: group, Kind: kind},
			GroupVersionResource: schema.GroupVersionResource{Group: group, Version: version, Resource: resource},
		}
	}
	got := APIVersionStrings([]APIResourceInfo{
		info("", "v1", "pods", "Pod"), info("apps", "v1", "deployments", "Deployment"),
		info("apps", "v1", "statefulsets", "StatefulSet"), info("", "v1", "pods", "Pod"),
	})
	assert.Equal(t, []string{"apps/v1", "apps/v1/Deployment", "apps/v1/StatefulSet", "v1", "v1/Pod"}, got)
	assert.Empty(t, APIVersionStrings(nil))
}

// stubDiscovery answers with fixed discovery documents.
type stubDiscovery struct {
	discovery.DiscoveryInterface
	version    *k8sversion.Info
	versionErr error
	groups     []*metav1.APIGroup
	lists      []*metav1.APIResourceList
	listsErr   error
}

func (s stubDiscovery) ServerVersion() (*k8sversion.Info, error) { return s.version, s.versionErr }

func (s stubDiscovery) ServerGroupsAndResources() ([]*metav1.APIGroup, []*metav1.APIResourceList, error) {
	return s.groups, s.lists, s.listsErr
}

func TestServedAPIResources(t *testing.T) {
	t.Parallel()
	lw := []string{"get", "list", "watch"}
	stub := stubDiscovery{
		version: &k8sversion.Info{GitVersion: "v1.30.2-eks-1"},
		groups: []*metav1.APIGroup{
			{Name: "", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "v1", Version: "v1"}}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "v1", Version: "v1"}},
			{Name: "autoscaling", Versions: []metav1.GroupVersionForDiscovery{
				{GroupVersion: "autoscaling/v1", Version: "v1"}, {GroupVersion: "autoscaling/v2", Version: "v2"},
			}, PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "autoscaling/v2", Version: "v2"}},
			{Name: "coordination.k8s.io", Versions: []metav1.GroupVersionForDiscovery{{GroupVersion: "coordination.k8s.io/v1", Version: "v1"}},
				PreferredVersion: metav1.GroupVersionForDiscovery{GroupVersion: "coordination.k8s.io/v1", Version: "v1"}},
		},
		lists: []*metav1.APIResourceList{
			{GroupVersion: "v1", APIResources: []metav1.APIResource{
				{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: lw},
				{Name: "pods/log", Kind: "Pod", Namespaced: true, Verbs: []string{"get"}},
				{Name: "bindings", Kind: "Binding", Namespaced: true, Verbs: []string{"create"}},
				{Name: "events", Kind: "Event", Namespaced: true, Verbs: lw},
			}},
			{GroupVersion: "autoscaling/v1", APIResources: []metav1.APIResource{
				{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, Verbs: lw},
			}},
			{GroupVersion: "autoscaling/v2", APIResources: []metav1.APIResource{
				{Name: "horizontalpodautoscalers", Kind: "HorizontalPodAutoscaler", Namespaced: true, Verbs: lw},
			}},
			{GroupVersion: "coordination.k8s.io/v1", APIResources: []metav1.APIResource{
				{Name: "leases", Kind: "Lease", Namespaced: true, Verbs: lw},
			}},
		},
	}

	version, resources, err := servedAPIResources(stub)
	require.NoError(t, err)
	assert.Equal(t, "v1.30.2", version, "the form the cache gives")
	assert.Equal(t, []string{
		"autoscaling/v1", "autoscaling/v1/HorizontalPodAutoscaler",
		"autoscaling/v2", "autoscaling/v2/HorizontalPodAutoscaler",
		"coordination.k8s.io/v1", "coordination.k8s.io/v1/Lease",
		"v1", "v1/ConfigMap", "v1/Event",
	}, APIVersionStrings(resources), "every served version with list+watch, and the kinds a cache does not watch")

	cached, err := discoverAPIs(stub, &Settings{ResourceFilter: excludeFilter{{Kind: "ConfigMap"}: true}}, "https://cluster")
	require.NoError(t, err)
	assert.Equal(t, APIVersionStrings(cached.all), APIVersionStrings(resources), "what a cache of the cluster reports")

	partial := stub
	partial.lists = stub.lists[:1]
	partial.listsErr = &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("no endpoints")}}
	_, resources, err = servedAPIResources(partial)
	require.NoError(t, err, "as in a full sync, one failed group version is left out")
	assert.Equal(t, []string{"v1", "v1/ConfigMap", "v1/Event"}, APIVersionStrings(resources))

	refused := errors.New("connection refused")
	_, _, err = servedAPIResources(stubDiscovery{versionErr: refused})
	require.ErrorIs(t, err, refused)

	failed := stub
	failed.lists, failed.listsErr = nil, refused
	_, _, err = servedAPIResources(failed)
	require.ErrorIs(t, err, refused)

	allFailed := partial
	allFailed.lists = nil
	_, _, err = servedAPIResources(allFailed)
	var gdf *discovery.ErrGroupDiscoveryFailed
	require.ErrorAs(t, err, &gdf, "nothing served is a failure")
}

// discoveryServer serves the legacy discovery documents of a cluster with one API group.
func discoveryServer(t *testing.T) *httptest.Server {
	t.Helper()
	lw := metav1.Verbs{"get", "list", "watch"}
	gv := metav1.GroupVersionForDiscovery{GroupVersion: "example.com/v1", Version: "v1"}
	docs := map[string]any{
		"/version": k8sversion.Info{GitVersion: "v1.30.2-eks-1"},
		"/api":     metav1.APIVersions{Versions: []string{"v1"}},
		"/apis":    metav1.APIGroupList{Groups: []metav1.APIGroup{{Name: "example.com", Versions: []metav1.GroupVersionForDiscovery{gv}, PreferredVersion: gv}}},
		"/api/v1": metav1.APIResourceList{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "configmaps", Kind: "ConfigMap", Namespaced: true, Verbs: lw},
		}},
		"/apis/example.com/v1": metav1.APIResourceList{GroupVersion: "example.com/v1", APIResources: []metav1.APIResource{
			{Name: "widgets", Kind: "Widget", Namespaced: true, Verbs: lw},
		}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc, ok := docs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Warning", `299 - "deprecated"`)
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// warningCounter is a rest.WarningHandler.
type warningCounter struct{ n atomic.Int64 }

func (w *warningCounter) HandleWarningHeader(int, string, string) { w.n.Add(1) }

func TestDiscoverAPIResources(t *testing.T) {
	t.Parallel()
	warnings := &warningCounter{}
	cfg := &rest.Config{Host: discoveryServer(t).URL, WarningHandler: warnings}
	for range 2 {
		version, resources, err := DiscoverAPIResources(t.Context(), cfg)
		require.NoError(t, err)
		assert.Equal(t, "v1.30.2", version)
		assert.Equal(t, []string{"example.com/v1", "example.com/v1/Widget", "v1", "v1/ConfigMap"}, APIVersionStrings(resources))
	}
	assert.Nil(t, cfg.WrapTransport, "a configuration the caller may use again stays as it was")
	assert.Zero(t, warnings.n.Load(), "as in a cache of the cluster, its warnings are not logged")
}

// roundTripFunc is an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// One deadline covers every request of a discovery, so that a slow cluster costs discoveryTimeout
// and not that much per request.
func TestDiscoverAPIResourcesBoundsItsRequests(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	deadlines := map[time.Time]int{}
	cfg := &rest.Config{Host: discoveryServer(t).URL}
	cfg.Wrap(func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if d, ok := r.Context().Deadline(); ok {
				mu.Lock()
				deadlines[d]++
				mu.Unlock()
			}
			return next.RoundTrip(r)
		})
	})

	start := time.Now()
	_, _, err := DiscoverAPIResources(t.Context(), cfg)
	end := time.Now()
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, deadlines, 1, "one deadline for the discovery")
	for d, requests := range deadlines {
		assert.GreaterOrEqual(t, requests, 5, "/version, /api, /apis and both group versions")
		assert.WithinRange(t, d, start.Add(discoveryTimeout), end.Add(discoveryTimeout))
	}
}

// The discovery client takes no context: the request's context still has to stop it, or a reader who
// gives up leaves it waiting on a cluster that does not answer.
func TestDiscoverAPIResourcesStopsWithItsContext(t *testing.T) {
	t.Parallel()
	arrived := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(arrived) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, err := DiscoverAPIResources(ctx, &rest.Config{Host: srv.URL})
		result <- err
	}()
	select {
	case <-arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("no request reached the cluster")
	}
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("the discovery outlived its context")
	}
}

func TestNormalizeServerVersion(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"v1.36.0":          "v1.36.0",
		"v1.30.2-eks-1234": "v1.30.2",
		"v1.29.4+k3s1":     "v1.29.4",
		"v1.28.3-gke.100":  "v1.28.3",
		"not-a-version":    "not-a-version",
	} {
		assert.Equal(t, want, normalizeServerVersion(in), in)
	}
}
