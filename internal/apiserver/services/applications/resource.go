package applications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	apirest "github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/compare"
	"github.com/juliosaraiva/patrol-cd/internal/diff"
	"github.com/juliosaraiva/patrol-cd/internal/project"
	"github.com/juliosaraiva/patrol-cd/internal/rbac"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	pubapi "github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

// clusterTarget is the destination cluster of an Application and the configuration of a
// call to it.
type clusterTarget struct {
	cluster *v1alpha1.Cluster
	config  *rest.Config
	// impersonated is the impersonated user ("" for the cluster's own credentials).
	impersonated string
}

// userMessager is implemented by errors of a ClusterConfigs provider whose text is meant for the
// caller (core mode: the caller owns the kubeconfig the message is about). Any other provider
// error stays out of the response: it can name credential Secrets and connection details.
type userMessager interface{ UserMessage() string }

// noCredentials is implemented by errors of a ClusterConfigs provider that holds no credentials for
// a cluster by design (core mode with no kubeconfig context for it). Nothing was sent to the cluster,
// and a call that can do without the cluster goes on.
type noCredentials interface{ NoCredentials() }

// clusterConfigUnavailable is the 503 for a destination cluster whose configuration cannot be
// built. The cause is attached for the server log; it reaches the client only when it opts in
// with UserMessage.
func clusterConfigUnavailable(server string, cause error) *apirest.Error {
	msg := fmt.Sprintf("error getting application cluster config: the configuration of cluster %s is not available", server)
	var um userMessager
	if errors.As(cause, &um) {
		msg += ": " + um.UserMessage()
	}
	return apirest.Unavailable("%s", msg).WithCause(cause)
}

// appClusterConfig resolves the destination cluster of app and its configuration (upstream
// getApplicationClusterConfig). With impersonate, the impersonation settings are read from
// the current snapshot: when enabled, the first spec.destinationServiceAccounts entry of
// proj matching the cluster URL and destination namespace is impersonated (user name only);
// with no match the request fails when application.sync.impersonation.enforced, otherwise
// the cluster's credentials are used (info log). Without impersonate (web terminal,
// SYNC-123, and a preview's question about the destination's versions), the cluster's
// credentials are always used.
func (s *Service) appClusterConfig(ctx context.Context, app *v1alpha1.Application, proj *v1alpha1.AppProject, st *settings.Settings, impersonate bool) (*clusterTarget, error) {
	cc, err := s.clusterConfigs()
	if err != nil {
		return nil, err
	}
	kl, _ := s.lookups(st)
	cluster, err := project.ResolveDestination(ctx, app.Spec.Destination, kl)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, apirest.InvalidArgument("error getting application cluster config: error validating destination: %v", err)
	}
	cfg, err := cc.RESTConfig(ctx, cluster)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		s.log().WarnContext(ctx, "applications: cannot build the destination cluster configuration", "server", cluster.Spec.Server, "error", err)
		return nil, clusterConfigUnavailable(cluster.Spec.Server, err)
	}
	cfg = rest.CopyConfig(cfg)
	t := &clusterTarget{cluster: cluster, config: cfg}
	if !impersonate {
		return t, nil
	}
	imp := st.Impersonation()
	if !imp.Enabled {
		return t, nil
	}
	sel, err := project.SelectServiceAccount(proj, app, cluster.Spec.Server, project.ImpersonationSettings{Enabled: imp.Enabled, Enforced: imp.Enforced})
	if err != nil {
		var nsa *project.NoServiceAccountError
		if errors.As(err, &nsa) {
			return nil, apirest.FailedPrecondition("error getting application cluster config: %s", nsa.Error())
		}
		return nil, apirest.FailedPrecondition("error getting application cluster config: error deriving service account to impersonate: %v", err)
	}
	if sel.UserName == "" {
		s.log().InfoContext(ctx, fmt.Sprintf("no matching service account found for impersonation for app %s/%s (project: %s, server: %s, namespace: %s), falling back to the cluster credentials",
			app.Namespace, app.Name, proj.Name, cluster.Spec.Server, app.Spec.Destination.Namespace))
		return t, nil
	}
	icfg, err := clusters.ImpersonatingConfig(cfg, sel.UserName)
	if err != nil {
		return nil, apirest.Internal(err)
	}
	t.config, t.impersonated = icfg, sel.UserName
	return t, nil
}

// destinationError maps an error of a call to a destination cluster: Kubernetes API
// errors keep their status and message (they describe the request on that cluster, as
// upstream reports them), prefixed with what failed; authentication failures and
// transport errors are reported generically and logged.
func (s *Service) destinationError(ctx context.Context, prefix string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var e *apirest.Error
	if errors.As(err, &e) {
		return e
	}
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		s.log().WarnContext(ctx, "applications: destination cluster call failed", "what", prefix, "error", err)
		return apirest.Unavailable("%s: the destination cluster is not reachable", prefix).WithCause(err)
	}
	msg := prefix + ": " + st.Status().Message
	mk := func(status int, code pubapi.ErrorCode) error {
		return &apirest.Error{Status: status, Code: code, Detail: msg, Cause: err}
	}
	switch {
	case apierrors.IsNotFound(err):
		return mk(http.StatusNotFound, pubapi.CodeNotFound)
	case apierrors.IsForbidden(err):
		return mk(http.StatusForbidden, pubapi.CodePermissionDenied)
	case apierrors.IsUnauthorized(err):
		s.log().WarnContext(ctx, "applications: destination cluster rejected the credentials", "what", prefix, "error", err)
		return apirest.Unavailable("%s: the destination cluster rejected the cluster credentials", prefix).WithCause(err)
	case apierrors.IsAlreadyExists(err):
		return mk(http.StatusConflict, pubapi.CodeAlreadyExists)
	case apierrors.IsConflict(err):
		return mk(http.StatusConflict, pubapi.CodeConflict)
	case apierrors.IsInvalid(err):
		return mk(http.StatusUnprocessableEntity, pubapi.CodeInvalidArgument)
	case apierrors.IsBadRequest(err), apierrors.IsMethodNotSupported(err), apierrors.IsNotAcceptable(err), apierrors.IsUnsupportedMediaType(err):
		return mk(http.StatusBadRequest, pubapi.CodeInvalidArgument)
	case apierrors.IsTooManyRequests(err):
		return mk(http.StatusTooManyRequests, pubapi.CodeResourceExhausted)
	case apierrors.IsRequestEntityTooLargeError(err):
		return mk(http.StatusRequestEntityTooLarge, pubapi.CodeResourceExhausted)
	case apierrors.IsGone(err), apierrors.IsResourceExpired(err):
		return mk(http.StatusGone, pubapi.CodeFailedPrecondition)
	case apierrors.IsServiceUnavailable(err), apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return mk(http.StatusServiceUnavailable, pubapi.CodeUnavailable)
	}
	return mk(http.StatusBadGateway, pubapi.CodeUnavailable)
}

// apiResource finds the API resource serving group/version/kind on the cluster of cfg
// (upstream kubectl ServerResourceForGroupVersionKind) and whether it is namespaced. An
// empty version selects the group's preferred version.
func apiResource(ctx context.Context, cfg *rest.Config, group, version, kind, verb string) (schema.GroupVersionResource, bool, string, error) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return schema.GroupVersionResource{}, false, "", err
	}
	if version == "" {
		if group == "" {
			version = "v1"
		} else {
			groups, err := dc.ServerGroups()
			if err != nil {
				return schema.GroupVersionResource{}, false, "", err
			}
			for _, g := range groups.Groups {
				if g.Name == group {
					version = g.PreferredVersion.Version
				}
			}
			if version == "" {
				return schema.GroupVersionResource{}, false, "", apierrors.NewNotFound(schema.GroupResource{Group: group, Resource: kind}, "")
			}
		}
	}
	gv := schema.GroupVersion{Group: group, Version: version}
	if err := ctx.Err(); err != nil {
		return schema.GroupVersionResource{}, false, "", err
	}
	list, err := dc.ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		return schema.GroupVersionResource{}, false, "", err
	}
	for _, r := range list.APIResources {
		if r.Kind != kind || strings.Contains(r.Name, "/") {
			continue
		}
		if verb != "" && len(r.Verbs) > 0 && !slices.Contains([]string(r.Verbs), verb) {
			continue
		}
		return gv.WithResource(r.Name), r.Namespaced, version, nil
	}
	return schema.GroupVersionResource{}, false, "", apierrors.NewNotFound(schema.GroupResource{Group: group, Resource: kind},
		fmt.Sprintf("no resource of kind %s in %s supports %s", kind, gv, verb))
}

// resourceClient returns the dynamic client of a resource (namespaced or not).
func resourceClient(cfg *rest.Config, gvr schema.GroupVersionResource, namespaced bool, namespace string) (dynamic.ResourceInterface, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	if namespaced {
		return dyn.Resource(gvr).Namespace(namespace), nil
	}
	return dyn.Resource(gvr), nil
}

// clusterCtx bounds a non-streaming destination cluster call.
func (s *Service) clusterCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, s.d.Resources.ClusterCallTimeout)
}

// getLiveObject reads a live object from the cluster of cfg.
func (s *Service) getLiveObject(ctx context.Context, cfg *rest.Config, group, version, kind, namespace, name string) (*unstructured.Unstructured, error) {
	ctx, cancel := s.clusterCtx(ctx)
	defer cancel()
	gvr, namespaced, _, err := apiResource(ctx, cfg, group, version, kind, "get")
	if err != nil {
		return nil, s.destinationError(ctx, "error getting resource", err)
	}
	ri, err := resourceClient(cfg, gvr, namespaced, namespace)
	if err != nil {
		return nil, s.destinationError(ctx, "error getting resource", err)
	}
	obj, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, s.destinationError(ctx, "error getting resource", err)
	}
	return obj, nil
}

// resourceQuery identifies a resource of an Application's tree.
type resourceQuery struct {
	name, appNamespace, project                   string
	group, version, kind, namespace, resourceName string
}

func (q resourceQuery) appRequest() appRequest {
	return appRequest{name: q.name, namespace: q.appNamespace, project: q.project}
}

func resourceQueryOf(r *pubapi.ApplicationResourceRequest) resourceQuery {
	return resourceQuery{name: r.Name, appNamespace: r.AppNamespace, project: r.Project,
		group: r.Group, version: r.Version, kind: r.Kind, namespace: r.Namespace, resourceName: r.ResourceName}
}

// liveTarget is an authorized request on a resource of an Application's tree.
type liveTarget struct {
	app     *v1alpha1.Application
	proj    *v1alpha1.AppProject
	st      *settings.Settings
	node    *compare.ResourceNode
	version string
	cluster *clusterTarget
	// action is the RBAC action that authorized the request.
	action string
}

// resourceAction returns the RBAC action authorizing verb on the resource of q:
// get/action as given; update and delete use the fine-grained
// "<verb>/<group>/<kind>/<namespace>/<name>" unless
// server.rbac.disableApplicationFineGrainedRBACInheritance is false and the caller holds the
// plain verb on the Application (pre-v3 behavior, upstream getAppLiveResource).
func (s *Service) resourceAction(ctx context.Context, verb string, q resourceQuery, st *settings.Settings) string {
	if verb != rbac.ActionUpdate && verb != rbac.ActionDelete {
		return verb
	}
	fine := rbac.SubResourceAction(verb, q.group, q.kind, q.namespace, q.resourceName)
	if st.ApplicationFineGrainedRBACInheritanceDisabled() {
		return fine
	}
	ns := s.nsOrDefault(q.appNamespace)
	object := ""
	if q.project != "" {
		object = rbac.AppObject(s.controlNS(), q.project, ns, q.name)
	} else if c, err := s.kube(); err == nil && s.namespaceEnabled(ns) {
		// The read only decides which action string is checked; the check itself (with the
		// existence-hiding rules) is getApp's.
		app := &v1alpha1.Application{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: q.name}, app); err == nil {
			object = s.rbacObject(app)
		}
	}
	if object != "" && apirest.Can(ctx, rbac.ResourceApplications, verb, object) {
		return verb
	}
	return fine
}

// resolveLiveTarget authorizes verb on the resource of q and checks that it is a node with a
// live object of the Application's tree (managed or orphaned); it then resolves the
// destination cluster configuration (impersonated when configured). The order is
// upstream's: RBAC, tree membership, cluster configuration.
func (s *Service) resolveLiveTarget(ctx context.Context, verb string, q resourceQuery) (*liveTarget, error) {
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	action := s.resourceAction(ctx, verb, q, st)
	app, proj, err := s.getApp(ctx, action, q.appRequest())
	if err != nil {
		return nil, err
	}
	if q.kind == "" || q.resourceName == "" {
		return nil, apirest.InvalidArgument("kind and resourceName are required")
	}
	tree, err := s.appTree(ctx, app, proj)
	if err != nil {
		return nil, err
	}
	node := findTreeNode(tree, q.group, q.kind, q.namespace, q.resourceName)
	if node == nil {
		return nil, notInTree(q.kind, q.group, q.resourceName, app.Name)
	}
	cc, err := s.appClusterConfig(ctx, app, proj, st, true)
	if err != nil {
		return nil, err
	}
	version := node.Version
	if q.version != "" {
		version = q.version
	}
	return &liveTarget{app: app, proj: proj, st: st, node: node, version: version, cluster: cc, action: action}, nil
}

// maskedManifest returns the JSON of obj with core Secret data and sensitive annotations
// masked.
func maskedManifest(obj *unstructured.Unstructured, st *settings.Settings) (string, error) {
	if obj == nil {
		return "", errors.New("manifest was nil")
	}
	if isCoreSecret(obj) {
		masked, _, err := diff.HideSecretData(obj, nil, sensitiveAnnotations(st))
		if err != nil {
			return "", fmt.Errorf("error replacing secret values: %w", err)
		}
		obj = masked
	}
	b, err := json.Marshal(obj.Object)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// getResource serves GET /applications/{name}/resource (applications, get): the live
// manifest of a resource of the Application's tree, read from the destination cluster with
// the Application's cluster configuration; Secret data masked.
func (s *Service) getResource(ctx context.Context, q *pubapi.ApplicationResourceRequest) (*pubapi.ApplicationResourceResponse, error) {
	lt, err := s.resolveLiveTarget(ctx, rbac.ActionGet, resourceQueryOf(q))
	if err != nil {
		return nil, err
	}
	n := lt.node
	obj, err := s.getLiveObject(ctx, lt.cluster.config, n.Group, lt.version, n.Kind, n.Namespace, n.Name)
	if err != nil {
		return nil, err
	}
	m, err := maskedManifest(obj, lt.st)
	if err != nil {
		return nil, apirest.Internal(err)
	}
	return &pubapi.ApplicationResourceResponse{Manifest: m}, nil
}

// resourcePatchType maps the patch type of a resource patch request.
func resourcePatchType(t string) (types.PatchType, error) {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "", pubapi.PatchTypeMerge, string(types.MergePatchType):
		return types.MergePatchType, nil
	case pubapi.PatchTypeJSON, string(types.JSONPatchType):
		return types.JSONPatchType, nil
	case pubapi.PatchTypeStrategic, string(types.StrategicMergePatchType):
		return types.StrategicMergePatchType, nil
	}
	return "", apirest.InvalidArgument("unsupported patch type %q (json, merge or strategic)", t)
}

// patchResource serves POST /applications/{name}/resource: patches a live resource of the
// Application's tree (applications, update/<group>/<kind>/<namespace>/<name>, or update
// with inheritance enabled) and returns the masked result. Errors patching a core Secret
// carry no detail (they may quote the patch).
func (s *Service) patchResource(ctx context.Context, req *pubapi.ApplicationResourcePatchRequest) (*pubapi.ApplicationResourceResponse, error) {
	pt, err := resourcePatchType(req.PatchType)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Patch) == "" || !json.Valid([]byte(req.Patch)) {
		return nil, apirest.InvalidArgument("patch must be a JSON document")
	}
	q := resourceQuery{name: req.Name, appNamespace: req.AppNamespace, project: req.Project,
		group: req.Group, version: req.Version, kind: req.Kind, namespace: req.Namespace, resourceName: req.ResourceName}
	lt, err := s.resolveLiveTarget(ctx, rbac.ActionUpdate, q)
	if err != nil {
		return nil, err
	}
	n := lt.node
	isSecret := n.Group == "" && n.Kind == "Secret"
	fail := func(err error) error {
		if isSecret && ctx.Err() == nil {
			e := s.destinationError(ctx, "error patching resource", err)
			pe := apirest.ProblemFor(e)
			return &apirest.Error{Status: pe.Status, Code: pe.Code, Detail: fmt.Sprintf("failed to patch Secret %s/%s", n.Namespace, n.Name)}
		}
		return s.destinationError(ctx, "error patching resource", err)
	}
	cctx, cancel := s.clusterCtx(ctx)
	defer cancel()
	gvr, namespaced, _, err := apiResource(cctx, lt.cluster.config, n.Group, lt.version, n.Kind, "patch")
	if err != nil {
		return nil, fail(err)
	}
	ri, err := resourceClient(lt.cluster.config, gvr, namespaced, n.Namespace)
	if err != nil {
		return nil, fail(err)
	}
	obj, err := ri.Patch(cctx, n.Name, pt, []byte(req.Patch), metav1.PatchOptions{})
	if err != nil {
		return nil, fail(err)
	}
	if obj == nil {
		return nil, apirest.Internal(errors.New("failed to patch resource: manifest was nil"))
	}
	m, err := maskedManifest(obj, lt.st)
	if err != nil {
		return nil, apirest.Internal(err)
	}
	auditApp(ctx, lt.app, lt.action, s.rbacObject(lt.app), apirest.ReasonResourceUpdated, "patched resource %s/%s '%s'", n.Group, n.Kind, n.Name)
	return &pubapi.ApplicationResourceResponse{Manifest: m}, nil
}

// deleteResource serves DELETE /applications/{name}/resource (applications,
// delete/<group>/<kind>/<namespace>/<name>): orphan → propagation Orphan, force →
// Background with a zero grace period, otherwise Foreground.
func (s *Service) deleteResource(ctx context.Context, req *pubapi.ApplicationResourceDeleteRequest) (*pubapi.ApplicationResponse, error) {
	q := resourceQuery{name: req.Name, appNamespace: req.AppNamespace, project: req.Project,
		group: req.Group, version: req.Version, kind: req.Kind, namespace: req.Namespace, resourceName: req.ResourceName}
	lt, err := s.resolveLiveTarget(ctx, rbac.ActionDelete, q)
	if err != nil {
		return nil, err
	}
	opts := deleteOptions(req.Orphan != nil && *req.Orphan, req.Force != nil && *req.Force)
	n := lt.node
	cctx, cancel := s.clusterCtx(ctx)
	defer cancel()
	gvr, namespaced, _, err := apiResource(cctx, lt.cluster.config, n.Group, lt.version, n.Kind, "delete")
	if err != nil {
		return nil, s.destinationError(ctx, "error deleting resource", err)
	}
	ri, err := resourceClient(lt.cluster.config, gvr, namespaced, n.Namespace)
	if err != nil {
		return nil, s.destinationError(ctx, "error deleting resource", err)
	}
	if err := ri.Delete(cctx, n.Name, opts); err != nil {
		return nil, s.destinationError(ctx, "error deleting resource", err)
	}
	auditApp(ctx, lt.app, lt.action, s.rbacObject(lt.app), apirest.ReasonResourceDeleted, "deleted resource %s/%s '%s'", n.Group, n.Kind, n.Name)
	return &pubapi.ApplicationResponse{}, nil
}

// deleteOptions returns the upstream propagation for the orphan/force flags.
func deleteOptions(orphan, force bool) metav1.DeleteOptions {
	switch {
	case orphan:
		p := metav1.DeletePropagationOrphan
		return metav1.DeleteOptions{PropagationPolicy: &p}
	case force:
		p := metav1.DeletePropagationBackground
		zero := int64(0)
		return metav1.DeleteOptions{PropagationPolicy: &p, GracePeriodSeconds: &zero}
	default:
		p := metav1.DeletePropagationForeground
		return metav1.DeleteOptions{PropagationPolicy: &p}
	}
}

// listEvents serves GET /applications/{name}/events (applications, get). Without a
// resource: the Events of the Application on the control plane (never impersonated). With
// resourceName/resourceUID: the resource must be a node of the tree (managed or orphaned)
// with that UID, name and namespace; its Events are read from the destination cluster
// with the Application's cluster configuration.
func (s *Service) listEvents(ctx context.Context, q *pubapi.ApplicationEventsQuery) (*corev1.EventList, error) {
	app, proj, err := s.getApp(ctx, rbac.ActionGet, appRequest{name: q.Name, namespace: q.AppNamespace, project: q.Project})
	if err != nil {
		return nil, err
	}
	limit := s.d.Resources.MaxEvents
	out := &corev1.EventList{Items: []corev1.Event{}}
	out.APIVersion, out.Kind = "v1", "EventList"
	keep := func(items []corev1.Event, name, namespace string, uid types.UID) {
		for _, e := range items {
			io := e.InvolvedObject
			if io.Name != name || io.Namespace != namespace || io.UID != uid {
				continue
			}
			e.ManagedFields = nil
			out.Items = append(out.Items, e)
		}
	}
	if q.ResourceName == "" && q.ResourceUID == "" {
		c, err := s.kube()
		if err != nil {
			return nil, err
		}
		var list corev1.EventList
		err = c.List(ctx, &list, client.InNamespace(app.Namespace),
			client.MatchingFields{"involvedObject.name": app.Name, "involvedObject.namespace": app.Namespace, "involvedObject.uid": string(app.UID)},
			client.Limit(limit))
		if err != nil {
			return nil, fmt.Errorf("error listing resource events: %w", err)
		}
		out.ResourceVersion = list.ResourceVersion
		keep(list.Items, app.Name, app.Namespace, app.UID)
		return out, nil
	}
	st, err := s.settings()
	if err != nil {
		return nil, err
	}
	tree, err := s.appTree(ctx, app, proj)
	if err != nil {
		return nil, err
	}
	found := false
	for _, list := range [][]compare.ResourceNode{tree.Nodes, tree.OrphanedNodes} {
		for i := range list {
			n := &list[i]
			if n.UID == q.ResourceUID && n.Name == q.ResourceName && n.Namespace == q.ResourceNamespace {
				found = true
			}
		}
	}
	if !found {
		return nil, apirest.InvalidArgument("%s not found as part of application %s", q.ResourceName, app.Name)
	}
	cc, err := s.appClusterConfig(ctx, app, proj, st, true)
	if err != nil {
		return nil, err
	}
	kc, err := kubernetes.NewForConfig(cc.config)
	if err != nil {
		return nil, s.destinationError(ctx, "error listing resource events", err)
	}
	sel := fields.SelectorFromSet(fields.Set{
		"involvedObject.name":      q.ResourceName,
		"involvedObject.uid":       q.ResourceUID,
		"involvedObject.namespace": q.ResourceNamespace,
	}).String()
	cctx, cancel := s.clusterCtx(ctx)
	defer cancel()
	list, err := kc.CoreV1().Events(q.ResourceNamespace).List(cctx, metav1.ListOptions{FieldSelector: sel, Limit: limit})
	if err != nil {
		return nil, s.destinationError(ctx, "error listing resource events", err)
	}
	out.ResourceVersion = list.ResourceVersion
	keep(list.Items, q.ResourceName, q.ResourceNamespace, types.UID(q.ResourceUID))
	return out, nil
}
