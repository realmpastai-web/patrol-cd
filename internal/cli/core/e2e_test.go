package core_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/cli/config"
)

const cmGuestbook = `apiVersion: v1
kind: ConfigMap
metadata:
  name: guestbook-cfg
data:
  greeting: hello
`

// TestCoreAppLifecycle drives version, project, repository and application commands in
// core mode with a user that has plain Kubernetes RBAC on the Patrol namespace (no
// impersonate verb) while patrol-rbac-cm grants nothing: create, list, get, diff, sync
// through an Operation, wait, diff again and delete.
func TestCoreAppLifecycle(t *testing.T) {
	e := newCoreEnv(t)
	e.repo.set(t, "guestbook", cmGuestbook)
	kc, user := e.user("operator", operatorRules()...)

	e.run(kc, "version").Succeeded(t).Contains(t, "patrol: ", "patrol-server: ", "mode: core")
	e.run(kc, "account", "get-user-info").Succeeded(t).Contains(t, "Logged In: true", user)

	// Projects.
	e.run(kc, "proj", "create", "team-a", "-d", inCluster+","+e.dest, "-s", repoURL).Succeeded(t)
	e.run(kc, "proj", "list").Succeeded(t).Contains(t, "team-a", "default")
	e.run(kc, "proj", "get", "team-a").Succeeded(t).Contains(t, repoURL)
	e.run(kc, "proj", "delete", "team-a").Succeeded(t)

	// Repositories: the connection test runs on the repo-server.
	e.run(kc, "repo", "add", repoURL).Succeeded(t).Contains(t, "Repository '"+repoURL+"' added")
	e.run(kc, "repo", "list").Succeeded(t).Contains(t, repoURL, "Successful")
	require.NotEmpty(t, e.repo.Tested())

	// Applications.
	create := []string{"app", "create", "guestbook", "--repo", repoURL, "--path", "guestbook",
		"--dest-server", inCluster, "--dest-namespace", e.dest, "--project", "default"}
	e.run(kc, create...).Succeeded(t).Contains(t, "application 'guestbook' created")
	e.waitFor("guestbook reconciled", func() bool {
		a := e.app("guestbook")
		return a != nil && a.Status.ReconciledAt != nil
	})
	e.run(kc, "app", "list").Succeeded(t).Contains(t, e.ns+"/guestbook", "OutOfSync")
	// Server connection flags have no effect in core mode.
	e.run(kc, "--grpc-web-root-path", "/patrol", "--plaintext", "app", "list", "-o", "name").Succeeded(t).Contains(t, e.ns+"/guestbook")
	e.run(kc, "app", "get", "guestbook").Succeeded(t).Contains(t, "Name:", e.ns+"/guestbook", "Repo:", repoURL)
	r := e.run(kc, "app", "diff", "guestbook")
	require.Equal(t, 1, r.Code, r.Stderr)
	assert.Contains(t, r.Stdout, "greeting: hello")

	e.run(kc, "app", "sync", "guestbook", "--timeout", "60").Succeeded(t).
		Contains(t, "Phase:", "Succeeded", "configmap/guestbook-cfg created")
	e.run(kc, "app", "wait", "guestbook", "--sync", "--health", "--timeout", "60").Succeeded(t).Contains(t, "Synced", "Healthy")
	e.run(kc, "app", "diff", "guestbook").Succeeded(t)

	// The Operation records the caller's Kubernetes identity (SelfSubjectReview) and was
	// admitted by the admission policy without the impersonate verb.
	var ops v1alpha1.OperationList
	require.NoError(t, e.admin.List(e.ctx, &ops, client.InNamespace(e.ns)))
	require.Len(t, ops.Items, 1)
	assert.Equal(t, user, ops.Items[0].Spec.InitiatedBy.Username)
	assert.False(t, ops.Items[0].Spec.InitiatedBy.Automated)
	assert.False(t, e.can(kc, "impersonate", "operations"), "the operator must not have the impersonate verb")

	e.run(kc, "app", "delete", "guestbook", "-y").Succeeded(t)
	e.waitFor("guestbook deleted", func() bool { return e.app("guestbook") == nil })

	// Core mode never stores a token.
	c, err := config.Read(e.runner.ConfigPath())
	require.NoError(t, err)
	if c != nil {
		for _, u := range c.Users {
			assert.Empty(t, u.AuthToken)
		}
	}

	// Every request of the in-process server came from the caller, without impersonation.
	seen := map[string]bool{}
	for _, ev := range auditEvents(t, e.ns) {
		if !strings.HasSuffix(ev.UserAgent, coreAgentSuffix) {
			continue
		}
		seen[ev.Verb+" "+ev.ObjectRef.Resource] = true
		assert.Equal(t, user, ev.User.Username, "%s %s/%s", ev.Verb, ev.ObjectRef.Resource, ev.ObjectRef.Name)
		assert.Nil(t, ev.ImpersonatedUser)
	}
	for _, want := range []string{"create applications", "create operations", "delete applications", "get secrets", "create repositories"} {
		assert.True(t, seen[want], "no audited %q request of the in-process server", want)
	}
}

// can asks the API server whether the user of kubeconfig may use verb on resource of the
// Patrol API group in the test namespace.
func (e *coreEnv) can(kubeconfig, verb, resource string) bool {
	e.t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	require.NoError(e.t, err)
	kube, err := kubernetes.NewForConfig(cfg)
	require.NoError(e.t, err)
	res, err := kube.AuthorizationV1().SelfSubjectAccessReviews().Create(e.ctx, &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: e.ns, Verb: verb, Group: v1alpha1.GroupVersion.Group, Resource: resource,
		}},
	}, metav1.CreateOptions{})
	require.NoError(e.t, err)
	return res.Status.Allowed
}

// TestCoreKubernetesRBACIsTheAuthorization: a user limited to get/list on Applications can
// list and read them although Patrol RBAC grants nothing, and gets the Kubernetes
// forbidden error for anything else; no request of the in-process server used another
// identity or impersonation.
func TestCoreKubernetesRBACIsTheAuthorization(t *testing.T) {
	e := newCoreEnv(t)
	require.NoError(t, e.admin.Create(e.ctx, &v1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: "existing"},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "default",
			Source:      &v1alpha1.ApplicationSource{RepoURL: repoURL, Path: "guestbook"},
			Destination: v1alpha1.ApplicationDestination{Server: inCluster, Namespace: e.dest},
		},
	}))
	kc, viewer := e.user("viewer", rbacv1.PolicyRule{
		APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"applications"}, Verbs: []string{"get", "list"},
	})

	// Listing needs nothing but list on Applications; patrol-rbac-cm grants nothing.
	r := e.run(kc, "app", "list", "-o", "name").Succeeded(t).Contains(t, e.ns+"/existing")
	assert.Contains(t, r.Stderr, "patrol-cm is not readable with your Kubernetes credentials")

	forbidden := `is forbidden: User "` + viewer + `" cannot`
	// Reading one Application also reads its AppProject, which the user may not get.
	e.run(kc, "app", "get", "existing").Failed(t, 20, forbidden, `cannot get resource "appprojects"`)
	e.run(kc, "app", "create", "new", "--repo", repoURL, "--path", "guestbook", "--dest-server", inCluster,
		"--dest-namespace", e.dest, "--project", "default", "--validate=false").Failed(t, 20, forbidden)
	e.run(kc, "app", "delete", "existing", "-y").Failed(t, 20, forbidden)
	e.run(kc, "proj", "list").Failed(t, 20, forbidden, `cannot list resource "appprojects"`)
	require.NotNil(t, e.app("existing"))

	// With read access to every Patrol object, the create itself reaches Kubernetes and is
	// denied.
	kc2, reader := e.user("reader", rbacv1.PolicyRule{
		APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"*"}, Verbs: []string{"get", "list", "watch"},
	})
	e.run(kc2, "app", "get", "existing").Succeeded(t).Contains(t, e.ns+"/existing")
	e.run(kc2, "app", "create", "new", "--repo", repoURL, "--path", "guestbook", "--dest-server", inCluster,
		"--dest-namespace", e.dest, "--project", "default", "--validate=false").
		Failed(t, 20, `is forbidden: User "`+reader+`" cannot create resource "applications"`)
	assert.Nil(t, e.app("new"))

	var core int
	for _, ev := range auditEvents(t, e.ns) {
		if ev.ImpersonatedUser != nil {
			t.Errorf("request with impersonation: %+v", ev)
		}
		switch {
		case strings.HasSuffix(ev.UserAgent, coreAgentSuffix):
			core++
			assert.Contains(t, []string{viewer, reader}, ev.User.Username)
		case ev.UserAgent == harnessAgent:
		default:
			// Only the API server's own controllers may act in the namespace besides the
			// harness and the CLI.
			assert.True(t, strings.HasPrefix(ev.User.Username, "system:"), "unexpected identity %q (agent %q) in the namespace", ev.User.Username, ev.UserAgent)
		}
	}
	assert.Positive(t, core, "no audited request of the in-process server")
}

// A preview asks the destination only for what Cluster.status lacks, with the caller's credentials.
// A cluster the kubeconfig has no context for is not asked, as in a connection check: the render has
// what the status holds, and a sync selection or a diff goes on.
func TestCorePreviewVersions(t *testing.T) {
	const remote = "https://remote.example.com"
	e := newCoreEnv(t)
	e.repo.set(t, "guestbook", cmGuestbook)
	kc, _ := e.user("operator", operatorRules()...)
	cl := &v1alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: e.ns, Name: "remote"}, Spec: v1alpha1.ClusterSpec{Server: remote, Name: "remote"}}
	require.NoError(t, e.admin.Create(e.ctx, cl))
	cl.Status = v1alpha1.ClusterStatus{ServerVersion: "v1.30.0"}
	require.NoError(t, e.admin.Status().Update(e.ctx, cl))
	e.run(kc, "app", "create", "remote", "--repo", repoURL, "--path", "guestbook",
		"--dest-server", remote, "--dest-namespace", e.dest, "--project", "default", "--validate=false").Succeeded(t)

	r := e.run(kc, "app", "manifests", "remote", "--revision", "main")
	r.Succeeded(t)
	assert.Contains(t, r.Stdout, "ConfigMap")
	assert.Equal(t, "v1.30.0", e.repo.lastRender().KubeVersion)
	assert.Empty(t, e.repo.lastRender().APIVersions)
	assert.Equal(t, 1, e.run(kc, "app", "diff", "remote", "--revision", "main").Code, "the diff, not a failure")

	e.run(kc, "app", "create", "local", "--repo", repoURL, "--path", "guestbook",
		"--dest-server", "https://kubernetes.default.svc", "--dest-namespace", e.dest, "--project", "default", "--validate=false").Succeeded(t)
	e.run(kc, "app", "manifests", "local", "--revision", "main").Succeeded(t)
	assert.Regexp(t, `^v1\.\d+\.\d+$`, e.repo.lastRender().KubeVersion, "the selected context's cluster answered")
	assert.Contains(t, e.repo.lastRender().APIVersions, "v1/ConfigMap")
}
