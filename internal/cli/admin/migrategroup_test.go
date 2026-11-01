package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

func oldObject(kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": oldGroup + "/v1alpha1", "kind": kind, "metadata": map[string]any{"name": name}, "spec": spec,
	}}
}

func TestMigrateGroup(t *testing.T) {
	ctx := requireEnv(t)
	ns := newNamespace(t, ctx, "migrate")
	tio := newTestIO(ns)

	proj := oldObject("AppProject", "team-a", map[string]any{
		"sourceRepos":              []any{"*"},
		"destinations":             []any{map[string]any{"server": "*", "namespace": "*"}},
		"clusterResourceWhitelist": []any{map[string]any{"group": oldGroup, "kind": "Application"}},
	})
	proj.SetFinalizers([]string{"resources-finalizer." + oldGroup})
	proj = createObject(t, ctx, ns, proj)
	t.Cleanup(func() { removeFinalizers(t, ctx, proj) })
	repo := createObject(t, ctx, ns, oldObject("Repository", "apps", map[string]any{
		"url": "https://github.com/example/apps.git", "credentialsRef": map[string]any{"name": "apps-creds"},
	}))
	appSet := createObject(t, ctx, ns, oldObject("ApplicationSet", "set", map[string]any{
		"generators": []any{map[string]any{"list": map[string]any{"elements": []any{map[string]any{"name": "a"}}}}},
		"template": map[string]any{
			"metadata": map[string]any{"name": "{{name}}", "finalizers": []any{"resources-finalizer." + oldGroup}},
			"spec": map[string]any{
				"project": "team-a", "source": map[string]any{"repoURL": "https://github.com/example/apps.git", "path": ".", "targetRevision": "HEAD"},
				"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "a"},
			},
		},
	}))
	app := oldObject("Application", "a", map[string]any{
		"project": "team-a", "source": map[string]any{"repoURL": "https://github.com/example/apps.git", "path": ".", "targetRevision": "HEAD"},
		"destination": map[string]any{"server": "https://kubernetes.default.svc", "namespace": "a"},
	})
	app.SetAnnotations(map[string]string{"notifications." + oldGroup + "/subscribe.on-sync-succeeded.slack": "team", oldGroup + "/manifest-generate-paths": "."})
	app.SetLabels(map[string]string{"team": "a"})
	app.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: oldGroup + "/v1alpha1", Kind: "ApplicationSet", Name: "set", UID: appSet.GetUID(), Controller: new(true)}})
	createObject(t, ctx, ns, app)
	_, err := envKube.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "apps-creds", Labels: map[string]string{oldGroup + "/secret-type": "repository"},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: oldGroup + "/v1alpha1", Kind: "Repository", Name: "apps", UID: repo.GetUID()}},
		},
		StringData: map[string]string{"password": "migrate-test-password"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = envKube.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: settings.DefaultConfigMapName},
		Data:       map[string]string{"resource.customizations.health." + oldGroup + "_Application": "return {}"},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	get := func(kind patrolKind, name string) (*unstructured.Unstructured, error) {
		return envDyn.Resource(kind.gvr(currentGroup)).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	}

	// Dry run (default): the plan only.
	require.NoError(t, tio.run(ctx, "admin", "migrate-group", "--from-group", oldGroup))
	out := tio.out.String()
	assert.Contains(t, out, currentGroup+"/AppProject team-a in namespace "+ns+" created (dry run)")
	assert.Contains(t, out, currentGroup+"/Application a in namespace "+ns+" created (dry run)")
	assert.Contains(t, out, "/Secret apps-creds in namespace "+ns+": owner reference to "+currentGroup+"/Repository apps added (dry run)")
	assert.Contains(t, out, "Migration from "+oldGroup+" to "+currentGroup+": 4 created, 0 updated, 0 unchanged, 0 skipped (dry run)")
	assert.Contains(t, tio.errOut.String(), `WARNING: patrol-cm key "resource.customizations.health.`+oldGroup+`_Application" mentions `+oldGroup)
	assert.Contains(t, tio.errOut.String(), "Operations (sync history) are not copied")
	_, err = get(kindAppProject, "team-a")
	assert.True(t, apierrors.IsNotFound(err))

	// Copy.
	require.NoError(t, tio.run(ctx, "admin", "migrate-group", "--from-group", oldGroup, "--dry-run=false"))
	assert.Contains(t, tio.out.String(), "4 created, 0 updated, 0 unchanged, 0 skipped\n")
	newProj, err := get(kindAppProject, "team-a")
	require.NoError(t, err)
	t.Cleanup(func() { removeFinalizers(t, ctx, newProj) })
	assert.Equal(t, []string{"resources-finalizer." + currentGroup}, newProj.GetFinalizers())
	p := &v1alpha1.AppProject{}
	require.NoError(t, runtimeFromUnstructured(newProj, p))
	assert.Equal(t, currentGroup, p.Spec.ClusterResourceWhitelist[0].Group)

	newSet, err := get(kindApplicationSet, "set")
	require.NoError(t, err)
	set := &v1alpha1.ApplicationSet{}
	require.NoError(t, runtimeFromUnstructured(newSet, set))
	assert.Equal(t, []string{"resources-finalizer." + currentGroup}, set.Spec.Template.Finalizers)

	newApp, err := get(kindApplication, "a")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"notifications." + currentGroup + "/subscribe.on-sync-succeeded.slack": "team", currentGroup + "/manifest-generate-paths": "."}, newApp.GetAnnotations())
	assert.Equal(t, map[string]string{"team": "a"}, newApp.GetLabels())
	require.Len(t, newApp.GetOwnerReferences(), 1)
	owner := newApp.GetOwnerReferences()[0]
	assert.Equal(t, v1alpha1.GroupVersion.String(), owner.APIVersion)
	assert.Equal(t, newSet.GetUID(), owner.UID, "owner reference re-pointed to the copied ApplicationSet")

	newRepo, err := get(kindRepository, "apps")
	require.NoError(t, err)
	sec, err := envKube.CoreV1().Secrets(ns).Get(ctx, "apps-creds", metav1.GetOptions{})
	require.NoError(t, err)
	uids := map[types.UID]string{}
	for _, r := range sec.OwnerReferences {
		uids[r.UID] = r.APIVersion
	}
	assert.Equal(t, map[types.UID]string{repo.GetUID(): oldGroup + "/v1alpha1", newRepo.GetUID(): v1alpha1.GroupVersion.String()}, uids)
	assert.Equal(t, "repository", sec.Labels[v1alpha1.LabelKeySecretType])
	assert.Equal(t, "repository", sec.Labels[oldGroup+"/secret-type"], "source labels are kept")

	// Sources are untouched.
	src, err := envDyn.Resource(kindApplication.gvr(oldGroup)).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, src.GetAnnotations(), oldGroup+"/manifest-generate-paths")

	// Re-running is idempotent; changed sources need --overwrite.
	require.NoError(t, tio.run(ctx, "admin", "migrate-group", "--from-group", oldGroup, "--dry-run=false"))
	assert.Contains(t, tio.out.String(), "0 created, 0 updated, 4 unchanged, 0 skipped\n")
	assert.NotContains(t, tio.out.String(), "owner reference to")
	src.Object["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"] = "v2"
	_, err = envDyn.Resource(kindApplication.gvr(oldGroup)).Namespace(ns).Update(ctx, src, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, tio.run(ctx, "admin", "migrate-group", "--from-group", oldGroup, "--dry-run=false"))
	assert.Contains(t, tio.out.String(), currentGroup+"/Application a in namespace "+ns+" exists and differs; skipped (use --overwrite)")
	require.NoError(t, tio.run(ctx, "admin", "migrate-group", "--from-group", oldGroup, "--dry-run=false", "--overwrite"))
	assert.Contains(t, tio.out.String(), "0 created, 1 updated, 3 unchanged, 0 skipped\n")
	newApp, err = get(kindApplication, "a")
	require.NoError(t, err)
	rev, _, _ := unstructured.NestedString(newApp.Object, "spec", "source", "targetRevision")
	assert.Equal(t, "v2", rev)

	// --overwrite gives the copy the source's owners and no others.
	newApp.SetOwnerReferences(append(newApp.GetOwnerReferences(), metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid"}))
	_, err = envDyn.Resource(kindApplication.gvr(currentGroup)).Namespace(ns).Update(ctx, newApp, metav1.UpdateOptions{})
	require.NoError(t, err)
	src, err = envDyn.Resource(kindApplication.gvr(oldGroup)).Namespace(ns).Get(ctx, "a", metav1.GetOptions{})
	require.NoError(t, err)
	src.Object["spec"].(map[string]any)["source"].(map[string]any)["targetRevision"] = "v3"
	_, err = envDyn.Resource(kindApplication.gvr(oldGroup)).Namespace(ns).Update(ctx, src, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, tio.run(ctx, "admin", "migrate-group", "--from-group", oldGroup, "--dry-run=false", "--overwrite"))
	newApp, err = get(kindApplication, "a")
	require.NoError(t, err)
	owners := newApp.GetOwnerReferences()
	require.Len(t, owners, 1)
	assert.Equal(t, "ApplicationSet", owners[0].Kind)

	// Errors.
	require.ErrorContains(t, tio.run(ctx, "admin", "migrate-group", "--from-group", currentGroup), "must name two different API groups")
	require.ErrorContains(t, tio.run(ctx, "admin", "migrate-group", "--from-group", "argoproj.io"), "use 'patrol admin argocd-import'")
	require.ErrorContains(t, tio.run(ctx, "admin", "migrate-group", "--from-group", "missing.example.com"), "API group missing.example.com is not installed")
	require.ErrorContains(t, tio.run(ctx, "admin", "migrate-group"), `required flag(s) "from-group" not set`)
}
