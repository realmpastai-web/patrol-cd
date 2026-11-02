package compare

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cache"
	"github.com/juliosaraiva/patrol-cd/internal/health"
	"github.com/juliosaraiva/patrol-cd/internal/render"
	"github.com/juliosaraiva/patrol-cd/internal/tracking"
)

func infoOf(t *testing.T, manifest string, customLabels ...string) *ResourceInfo {
	t.Helper()
	res := &ResourceInfo{}
	populateNodeInfo(obj(t, manifest), res, customLabels)
	return res
}

func infoValue(info []InfoItem, name string) (string, bool) {
	for _, i := range info {
		if i.Name == name {
			return i.Value, true
		}
	}
	return "", false
}

func TestErrMessageAppPathDoesNotExistMatchesRender(t *testing.T) {
	t.Parallel()
	assert.Equal(t, render.ErrMessageAppPathDoesNotExist, errMessageAppPathDoesNotExist)
}

func TestPodInfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		manifest   string
		reason     string
		containers string
		restarts   string
		cpu        string
		mem        string
		noRequests bool
	}{
		{
			name: "running",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns, labels: {app: web}}
spec:
  nodeName: node-1
  containers:
  - name: c
    image: nginx:1
    resources: {requests: {cpu: 250m, memory: 64Mi}}
status:
  phase: Running
  containerStatuses:
  - {name: c, ready: true, restartCount: 2, state: {running: {}}}
`,
			reason: "Running", containers: "1/1", restarts: "2", cpu: "250", mem: "67108864000",
		},
		{
			name: "crash loop",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec: {containers: [{name: c, image: app:1}]}
status:
  phase: Running
  containerStatuses:
  - {name: c, ready: false, restartCount: 5, state: {waiting: {reason: CrashLoopBackOff}}}
`,
			reason: "CrashLoopBackOff", containers: "0/1", restarts: "5", cpu: "0", mem: "0",
		},
		{
			name: "init waiting",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec:
  initContainers: [{name: i, image: init:1}]
  containers: [{name: c, image: app:1}]
status:
  phase: Pending
  initContainerStatuses:
  - {name: i, state: {waiting: {reason: ImagePullBackOff}}}
`,
			reason: "Init:ImagePullBackOff", containers: "0/1", cpu: "0", mem: "0",
		},
		{
			name: "init exit code",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec:
  initContainers: [{name: i, image: init:1}]
  containers: [{name: c, image: app:1}]
status:
  phase: Pending
  initContainerStatuses:
  - {name: i, state: {terminated: {exitCode: 3}}}
`,
			reason: "Init:ExitCode:3", containers: "0/1", cpu: "0", mem: "0",
		},
		{
			name: "init progress",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec:
  initContainers: [{name: a, image: a:1}, {name: b, image: b:1}]
  containers: [{name: c, image: app:1}]
status:
  phase: Pending
  initContainerStatuses:
  - {name: a, state: {terminated: {exitCode: 0}}}
  - {name: b, state: {running: {}}}
`,
			reason: "Init:1/2", containers: "0/1", cpu: "0", mem: "0",
		},
		{
			name: "sidecar counts as container",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec:
  initContainers: [{name: s, image: proxy:1, restartPolicy: Always, resources: {requests: {cpu: 100m}}}]
  containers: [{name: c, image: app:1, resources: {requests: {cpu: 200m}}}]
status:
  phase: Running
  conditions: [{type: Initialized, status: "True"}]
  initContainerStatuses:
  - {name: s, started: true, ready: true, state: {running: {}}}
  containerStatuses:
  - {name: c, ready: true, state: {running: {}}}
`,
			reason: "Running", containers: "2/2", cpu: "300", mem: "0",
		},
		{
			name: "completed with running sidecar-less container",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec: {containers: [{name: a, image: a:1}, {name: b, image: b:1}]}
status:
  phase: Running
  containerStatuses:
  - {name: a, ready: true, state: {running: {}}}
  - {name: b, ready: false, state: {terminated: {exitCode: 0, reason: Completed}}}
`,
			reason: "Running", containers: "1/2", cpu: "0", mem: "0",
		},
		{
			name: "signal",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec: {containers: [{name: a, image: a:1}]}
status:
  phase: Running
  containerStatuses:
  - {name: a, state: {terminated: {exitCode: 137, signal: 9}}}
`,
			reason: "Signal:9", containers: "0/1", cpu: "0", mem: "0",
		},
		{
			name: "succeeded has no requests",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec: {containers: [{name: a, image: a:1, resources: {requests: {cpu: "1"}}}]}
status:
  phase: Succeeded
  containerStatuses:
  - {name: a, state: {terminated: {exitCode: 0, reason: Completed}}}
`,
			reason: "Completed", containers: "0/1", noRequests: true,
		},
		{
			name: "terminating",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns, deletionTimestamp: "2026-01-01T00:00:00Z"}
spec: {containers: [{name: a, image: a:1}]}
status: {phase: Running}
`,
			reason: "Terminating", containers: "0/1", cpu: "0", mem: "0",
		},
		{
			name: "node lost",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns, deletionTimestamp: "2026-01-01T00:00:00Z"}
spec: {containers: [{name: a, image: a:1}]}
status: {phase: Running, reason: NodeLost}
`,
			reason: "Unknown", containers: "0/1", cpu: "0", mem: "0",
		},
		{
			name: "scheduling gated",
			manifest: `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns}
spec: {containers: [{name: a, image: a:1}]}
status:
  phase: Pending
  conditions: [{type: PodScheduled, status: "False", reason: SchedulingGated}]
`,
			reason: "SchedulingGated", containers: "0/1", cpu: "0", mem: "0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info := infoOf(t, tt.manifest)
			reason, _ := infoValue(info.Info, InfoStatusReason)
			assert.Equal(t, tt.reason, reason)
			containers, _ := infoValue(info.Info, InfoContainers)
			assert.Equal(t, tt.containers, containers)
			restarts, ok := infoValue(info.Info, InfoRestartCount)
			if tt.restarts == "" {
				assert.False(t, ok)
			} else {
				assert.Equal(t, tt.restarts, restarts)
			}
			cpu, okCPU := infoValue(info.Info, InfoRequestsCPU)
			mem, okMem := infoValue(info.Info, InfoRequestsMem)
			if tt.noRequests {
				assert.False(t, okCPU)
				assert.False(t, okMem)
			} else {
				assert.Equal(t, tt.cpu, cpu)
				assert.Equal(t, tt.mem, mem)
			}
			require.NotNil(t, info.PodInfo)
			require.NotNil(t, info.NetworkingInfo)
		})
	}
}

func TestPodInfoImagesNodeAndLabels(t *testing.T) {
	t.Parallel()
	info := infoOf(t, `
apiVersion: v1
kind: Pod
metadata: {name: p, namespace: ns, labels: {app: web}}
spec:
  nodeName: node-a
  initContainers: [{name: i, image: busybox:1}]
  containers: [{name: a, image: nginx:1}, {name: b, image: nginx:1}]
status: {phase: Running}
`)
	assert.Equal(t, []string{"busybox:1", "nginx:1"}, info.Images)
	node, _ := infoValue(info.Info, InfoNode)
	assert.Equal(t, "node-a", node)
	assert.Equal(t, map[string]string{"app": "web"}, info.NetworkingInfo.Labels)
	assert.Equal(t, "node-a", info.PodInfo.NodeName)
}

func TestPodRequests(t *testing.T) {
	t.Parallel()
	q := resource.MustParse
	always := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		InitContainers: []corev1.Container{
			{Name: "init-big", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("2")}}},
			{Name: "sidecar", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m"), corev1.ResourceMemory: q("10Mi")}}},
			{Name: "init-after", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: q("1Gi")}}},
		},
		Containers: []corev1.Container{
			{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("500m"), corev1.ResourceMemory: q("100Mi")}}},
		},
		Overhead: corev1.ResourceList{corev1.ResourceCPU: q("10m")},
	}}
	req := podRequests(pod)
	cpu := req[corev1.ResourceCPU]
	mem := req[corev1.ResourceMemory]
	// cpu: max(app 500m + sidecar 100m, init-big 2) + overhead 10m.
	assert.Equal(t, int64(2010), cpu.MilliValue())
	// memory: max(app 100Mi + sidecar 10Mi, sidecar 10Mi + init-after 1Gi).
	want := q("1Gi")
	want.Add(q("10Mi"))
	assert.Equal(t, want.Value(), mem.Value())

	// Resized containers report larger requests in their status.
	pod2 := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("100m")}}}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "app", Resources: &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: q("300m")}},
		}}},
	}
	cpu2 := podRequests(pod2)[corev1.ResourceCPU]
	assert.Equal(t, int64(300), cpu2.MilliValue())
}

func TestServiceInfo(t *testing.T) {
	t.Parallel()
	info := infoOf(t, `
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: ns
  annotations: {link.cd.patrol.example/docs: "Docs|https://docs.example.com", link.cd.patrol.example/xss: "javascript:alert(1)"}
spec: {type: LoadBalancer, selector: {app: web}}
status: {loadBalancer: {ingress: [{hostname: lb.example.com}, {ip: 10.0.0.1}]}}
`)
	require.NotNil(t, info.NetworkingInfo)
	assert.Equal(t, map[string]string{"app": "web"}, info.NetworkingInfo.TargetLabels)
	assert.Equal(t, []LoadBalancerIngress{{Hostname: "lb.example.com"}, {IP: "10.0.0.1"}}, info.NetworkingInfo.Ingress)
	assert.Equal(t, []string{"Docs|https://docs.example.com"}, info.NetworkingInfo.ExternalURLs)

	clusterIP := infoOf(t, `
apiVersion: v1
kind: Service
metadata: {name: web, namespace: ns}
spec: {selector: {app: web}}
status: {loadBalancer: {ingress: [{ip: 10.0.0.1}]}}
`)
	assert.Empty(t, clusterIP.NetworkingInfo.Ingress)
}

func TestIngressInfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		manifest string
		urls     []string
		targets  []string
	}{
		{
			name: "networking v1 with tls secret and wildcard path",
			manifest: `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: in, namespace: ns}
spec:
  tls: [{secretName: tls}]
  rules:
  - host: app.example.com
    http:
      paths:
      - path: /api*
        backend: {service: {name: api, port: {number: 80}}}
`,
			urls:    []string{"https://app.example.com/api"},
			targets: []string{"/Service/ns/api"},
		},
		{
			name: "extensions without tls uses load balancer host",
			manifest: `
apiVersion: extensions/v1beta1
kind: Ingress
metadata: {name: in, namespace: ns}
spec:
  backend: {serviceName: default-svc}
  rules:
  - http:
      paths:
      - path: /
        backend: {serviceName: web}
status: {loadBalancer: {ingress: [{ip: 1.2.3.4}]}}
`,
			urls:    []string{"http://1.2.3.4/"},
			targets: []string{"/Service/ns/default-svc", "/Service/ns/web"},
		},
		{
			name: "tls hosts list does not switch the scheme (upstream quirk)",
			manifest: `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: in, namespace: ns}
spec:
  tls: [{hosts: [app.example.com]}]
  rules:
  - host: app.example.com
    http: {paths: [{path: /, backend: {service: {name: web}}}]}
`,
			urls:    []string{"http://app.example.com/"},
			targets: []string{"/Service/ns/web"},
		},
		{
			name: "ignore default links keeps link annotations",
			manifest: `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: in
  namespace: ns
  annotations: {cd.patrol.example/ignore-default-links: "true", link.cd.patrol.example/x: "https://x.example.com"}
spec:
  rules:
  - host: app.example.com
    http: {paths: [{path: /, backend: {service: {name: web}}}]}
`,
			urls:    []string{"https://x.example.com"},
			targets: []string{"/Service/ns/web"},
		},
		{
			name: "no host and no load balancer yields no url",
			manifest: `
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata: {name: in, namespace: ns}
spec:
  rules:
  - http: {paths: [{path: /, backend: {service: {name: web}}}]}
`,
			targets: []string{"/Service/ns/web"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			info := infoOf(t, tt.manifest)
			require.NotNil(t, info.NetworkingInfo)
			assert.Equal(t, tt.urls, info.NetworkingInfo.ExternalURLs)
			var targets []string
			for _, r := range info.NetworkingInfo.TargetRefs {
				targets = append(targets, r.FullName())
			}
			assert.Equal(t, tt.targets, targets)
		})
	}
}

func TestGatewayAndRouteInfo(t *testing.T) {
	t.Parallel()
	gw := infoOf(t, `
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: gw, namespace: ns}
spec:
  listeners:
  - {name: http, protocol: HTTP, port: 80}
  - {name: https, protocol: HTTPS, port: 8443, hostname: secure.example.com}
status:
  addresses: [{type: IPAddress, value: 10.1.1.1}, {type: Hostname, value: gw.example.com}, {value: ""}]
`)
	assert.Equal(t, []LoadBalancerIngress{{IP: "10.1.1.1"}, {Hostname: "gw.example.com"}}, gw.NetworkingInfo.Ingress)
	assert.Equal(t, []string{"http://10.1.1.1", "http://gw.example.com", "https://secure.example.com:8443"}, gw.NetworkingInfo.ExternalURLs)

	route := infoOf(t, `
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: r, namespace: ns}
spec:
  parentRefs: [{name: gw}, {name: other, namespace: infra}]
  rules:
  - backendRefs: [{name: web, port: 80}, {name: ext, namespace: other, kind: ServiceImport, group: multicluster.x-k8s.io}]
`)
	var targets []string
	for _, r := range route.NetworkingInfo.TargetRefs {
		targets = append(targets, r.FullName())
	}
	assert.ElementsMatch(t, []string{
		"gateway.networking.k8s.io/Gateway/ns/gw",
		"gateway.networking.k8s.io/Gateway/infra/other",
		"/Service/ns/web",
		"multicluster.x-k8s.io/ServiceImport/other/ext",
	}, targets)
}

func TestIstioInfo(t *testing.T) {
	t.Parallel()
	vs := infoOf(t, `
apiVersion: networking.istio.io/v1
kind: VirtualService
metadata: {name: vs, namespace: ns}
spec:
  http:
  - route: [{destination: {host: reviews}}, {destination: {host: ratings.prod.svc.cluster.local}}]
`)
	var targets []string
	for _, r := range vs.NetworkingInfo.TargetRefs {
		targets = append(targets, r.FullName())
	}
	assert.ElementsMatch(t, []string{"/Service/ns/reviews", "/Service/prod/ratings"}, targets)

	se := infoOf(t, `
apiVersion: networking.istio.io/v1
kind: ServiceEntry
metadata: {name: se, namespace: ns, annotations: {link.cd.patrol.example/a: "https://a.example.com"}}
spec: {workloadSelector: {labels: {app: db}}}
`)
	assert.Equal(t, map[string]string{"app": "db"}, se.NetworkingInfo.TargetLabels)
	assert.Equal(t, []ResourceRef{{Kind: "Pod"}}, se.NetworkingInfo.TargetRefs)
	assert.Empty(t, se.NetworkingInfo.ExternalURLs, "upstream replaces networking info wholesale")
}

func TestRevisionCustomLabelsAndManagedByURL(t *testing.T) {
	t.Parallel()
	rs := infoOf(t, `
apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: rs, namespace: ns, annotations: {deployment.kubernetes.io/revision: "3"}, labels: {team: a, tier: web}}
`, "team", " tier")
	rev, _ := infoValue(rs.Info, InfoRevision)
	assert.Equal(t, "Rev:3", rev)
	team, ok := infoValue(rs.Info, "team")
	assert.True(t, ok)
	assert.Equal(t, "a", team)
	_, ok = infoValue(rs.Info, " tier")
	assert.False(t, ok, "custom labels are not trimmed")

	bad := infoOf(t, `
apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: rs, namespace: ns, annotations: {deployment.kubernetes.io/revision: "x"}}
`)
	_, ok = infoValue(bad.Info, InfoRevision)
	assert.False(t, ok)

	cr := infoOf(t, `
apiVersion: apps/v1
kind: ControllerRevision
metadata: {name: cr, namespace: ns}
revision: 7
`)
	rev, _ = infoValue(cr.Info, InfoRevision)
	assert.Equal(t, "Rev:7", rev)

	child := infoOf(t, `
apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata: {name: child, namespace: patrol, annotations: {cd.patrol.example/managed-by-url: "https://other.example.com"}}
`)
	url, _ := infoValue(child.Info, InfoManagedByURL)
	assert.Equal(t, "https://other.example.com", url)
}

func TestPopulateResourceInfoAttribution(t *testing.T) {
	t.Parallel()
	tr := tracking.New(tracking.Config{ControlPlaneNamespace: "patrol"})
	pop := PopulateResourceInfo(InfoConfig{Tracker: tr, Method: tracking.MethodAnnotation})
	cm := configMap("cm", "ns", nil)
	require.NoError(t, tr.SetAppInstance(cm, "my-app", "ns", tracking.MethodAnnotation, ""))

	info, keep := pop(cm, true)
	ri := info.(*ResourceInfo)
	assert.Equal(t, "my-app", ri.AppName)
	assert.True(t, keep, "tracked roots keep their manifest")

	info, keep = pop(cm, false)
	assert.Empty(t, info.(*ResourceInfo).AppName, "owned objects are attributed through their owners")
	assert.False(t, keep)

	crd := obj(t, `{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"x.example.com"}}`)
	_, keep = pop(crd, true)
	assert.True(t, keep, "CRDs are always retained")

	deploy := obj(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"d","namespace":"ns","generation":1},"spec":{"replicas":1},"status":{"observedGeneration":1,"replicas":1,"updatedReplicas":1,"availableReplicas":1}}`)
	info, _ = pop(deploy, true)
	require.NotNil(t, info.(*ResourceInfo).Health)
	assert.Equal(t, health.StatusHealthy, info.(*ResourceInfo).Health.Status)
}

// ownerGraph builds ConfigMaps in one namespace from each object's owners, in order, and from
// the Application each tracked object belongs to.
func ownerGraph(owners map[string][]string, apps map[string]string) map[ResourceKey]*cache.Resource {
	ns := map[ResourceKey]*cache.Resource{}
	add := func(name string) {
		key := cache.NewResourceKey("", "ConfigMap", "ns", name)
		if _, ok := ns[key]; !ok {
			ns[key] = &cache.Resource{Ref: cache.ResourceRef{ResourceKey: key}, Info: &ResourceInfo{AppName: apps[name]}}
		}
	}
	for name, refs := range owners {
		add(name)
		for _, owner := range refs {
			add(owner)
			r := ns[cache.NewResourceKey("", "ConfigMap", "ns", name)]
			r.OwnerRefs = append(r.OwnerRefs, metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: owner})
		}
	}
	return ns
}

func TestAppOfOwnerGraphs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		owners map[string][]string
		apps   map[string]string
		want   string
	}{
		{
			name:   "an owner shared by two chains is not a cycle",
			owners: map[string][]string{"r": {"a", "b"}, "a": {"c"}, "b": {"c", "d"}},
			apps:   map[string]string{"d": "app"},
			want:   "app",
		},
		{
			name:   "the first chain that reaches a tracked object decides",
			owners: map[string][]string{"r": {"a", "b"}, "a": {"c"}, "b": {"d"}},
			apps:   map[string]string{"c": "first", "d": "second"},
			want:   "first",
		},
		{
			name:   "no owner is tracked",
			owners: map[string][]string{"r": {"a", "b"}, "a": {"c"}, "b": {"c"}},
		},
		{
			name:   "a cycle stops the walk before a later owner",
			owners: map[string][]string{"r": {"a", "d"}, "a": {"b"}, "b": {"a"}},
			apps:   map[string]string{"d": "app"},
		},
		{
			name:   "a cycle after a shared owner stops the walk",
			owners: map[string][]string{"r": {"a", "b", "d"}, "a": {"c"}, "b": {"c", "e"}, "e": {"b"}},
			apps:   map[string]string{"d": "app"},
		},
		{
			name:   "a cycle after an owner that led nowhere stops the walk",
			owners: map[string][]string{"r": {"a", "d"}, "a": {"x", "b"}, "b": {"a"}},
			apps:   map[string]string{"d": "app"},
		},
		{
			name:   "a cycle through the first object stops the walk",
			owners: map[string][]string{"r": {"a", "d"}, "a": {"r"}},
			apps:   map[string]string{"d": "app"},
		},
		{
			name:   "an object that owns itself stops the walk",
			owners: map[string][]string{"r": {"r", "d"}},
			apps:   map[string]string{"d": "app"},
		},
		{
			name:   "a tracked object ends the chain before its own cycle",
			owners: map[string][]string{"r": {"a"}, "a": {"b"}, "b": {"a"}},
			apps:   map[string]string{"a": "app"},
			want:   "app",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ns := ownerGraph(tc.owners, tc.apps)
			assert.Equal(t, tc.want, AppOf(ns[cache.NewResourceKey("", "ConfigMap", "ns", "r")], ns))
		})
	}

	// An owner outside the namespace map is skipped.
	ns := ownerGraph(map[string][]string{"r": {"gone", "d"}}, map[string]string{"d": "app"})
	delete(ns, cache.NewResourceKey("", "ConfigMap", "ns", "gone"))
	assert.Equal(t, "app", AppOf(ns[cache.NewResourceKey("", "ConfigMap", "ns", "r")], ns))

	// An update starts from the old object, and a cycle leads back to its tracked copy in the map.
	ns = ownerGraph(map[string][]string{"r": {"a"}, "a": {"r"}}, map[string]string{"r": "app"})
	tracked := ns[cache.NewResourceKey("", "ConfigMap", "ns", "r")]
	old := &cache.Resource{Ref: tracked.Ref, OwnerRefs: tracked.OwnerRefs, Info: &ResourceInfo{}}
	assert.Equal(t, "app", AppOf(old, ns))
}

// TestAppOfLayeredOwners: both objects of a layer own both objects of the layer below, so the
// chains double with every layer while the objects grow by two. The walk has to search them
// all before it reaches the tracked owner, which only finishes if it visits each object once.
func TestAppOfLayeredOwners(t *testing.T) {
	t.Parallel()
	const layers = 64
	owners := map[string][]string{"r": {"a0", "d"}}
	for i := range layers - 1 {
		next := []string{fmt.Sprintf("a%d", i+1), fmt.Sprintf("b%d", i+1)}
		owners[fmt.Sprintf("a%d", i)] = next
		owners[fmt.Sprintf("b%d", i)] = next
	}
	ns := ownerGraph(owners, map[string]string{"d": "app"})
	got := make(chan string, 1)
	go func() { got <- AppOf(ns[cache.NewResourceKey("", "ConfigMap", "ns", "r")], ns) }()
	select {
	case name := <-got:
		assert.Equal(t, "app", name)
	case <-time.After(30 * time.Second):
		t.Fatal("the walk did not finish")
	}
}

func TestAppOfAndSkipResourceUpdate(t *testing.T) {
	t.Parallel()
	rsKey := cache.NewResourceKey("apps", "ReplicaSet", "ns", "rs")
	depKey := cache.NewResourceKey("apps", "Deployment", "ns", "d")
	podKey := cache.NewResourceKey("", "Pod", "ns", "p")
	dep := &cache.Resource{Ref: cache.ResourceRef{ResourceKey: depKey}, Info: &ResourceInfo{AppName: "app"}}
	rs := &cache.Resource{Ref: cache.ResourceRef{ResourceKey: rsKey}, OwnerRefs: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d"}}}
	pod := &cache.Resource{Ref: cache.ResourceRef{ResourceKey: podKey}, OwnerRefs: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "rs"}}}
	ns := map[ResourceKey]*cache.Resource{depKey: dep, rsKey: rs, podKey: pod}
	assert.Equal(t, "app", AppOf(pod, ns))

	// A cycle stops the walk.
	aKey := cache.NewResourceKey("", "ConfigMap", "ns", "a")
	bKey := cache.NewResourceKey("", "ConfigMap", "ns", "b")
	a := &cache.Resource{Ref: cache.ResourceRef{ResourceKey: aKey, UID: types.UID("a")}, OwnerRefs: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "b"}}}
	b := &cache.Resource{Ref: cache.ResourceRef{ResourceKey: bKey, UID: types.UID("b")}, OwnerRefs: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "a"}}}
	assert.Empty(t, AppOf(a, map[ResourceKey]*cache.Resource{aKey: a, bKey: b}))

	h := &health.HealthStatus{Status: health.StatusHealthy}
	assert.True(t, SkipResourceUpdate(&ResourceInfo{Health: h, ManifestHash: "x"}, &ResourceInfo{Health: h, ManifestHash: "x"}))
	assert.False(t, SkipResourceUpdate(&ResourceInfo{Health: h, ManifestHash: "x"}, &ResourceInfo{Health: h, ManifestHash: "y"}))
	assert.False(t, SkipResourceUpdate(&ResourceInfo{ManifestHash: "x"}, &ResourceInfo{Health: h, ManifestHash: "x"}))
	assert.False(t, SkipResourceUpdate(&ResourceInfo{}, &ResourceInfo{}), "empty hashes never skip")
}

func TestShouldHashManifest(t *testing.T) {
	t.Parallel()
	cm := configMap("cm", "ns", nil)
	assert.True(t, shouldHashManifest("app", "", "ConfigMap", cm))
	assert.False(t, shouldHashManifest("", "", "ConfigMap", cm))
	assert.True(t, shouldHashManifest("", v1alpha1.GroupName, "Application", cm))
	cm.SetAnnotations(map[string]string{v1alpha1.AnnotationKeyIgnoreResourceUpdates: "true"})
	assert.True(t, shouldHashManifest("", "", "ConfigMap", cm))
	cm.SetAnnotations(map[string]string{v1alpha1.AnnotationKeyIgnoreResourceUpdates: "yes"})
	assert.False(t, shouldHashManifest("", "", "ConfigMap", cm))
}
