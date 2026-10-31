package admin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"

	"github.com/juliosaraiva/patrol-cd/api/v1alpha1"
	"github.com/juliosaraiva/patrol-cd/internal/clusters"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
)

type importOptions struct {
	prune              bool
	dryRun             bool
	verbose            bool
	stopOperation      bool
	ignoreTracking     bool
	overrideOnConflict bool
	promptsEnabled     bool
	skipLabel          string
	appNamespaces      []string
	appSetNamespaces   []string
	namespaceScoped    bool
	migrateGroup       bool
	keepLiveClusters   bool
}

func newImportCommand(f *Factory, kube *kubeOptions) *cobra.Command {
	var o importOptions
	cmd := &cobra.Command{
		Use:   "import SOURCE",
		Short: "Import Patrol CD data from stdin (specify `-') or a file",
		Long: `Import a backup written by "patrol admin export" into the control-plane namespace.

Objects missing in the cluster are created, objects that differ are updated (labels,
annotations, finalizers and data or spec; status and other live fields are kept), equal
objects are left alone. Nothing is deleted unless --prune is given: live objects that are
not in the backup are reported as "needs pruning". Settings ConfigMaps, patrol-secret and
patrol-notifications-secret are never pruned. Secrets are pruned only when the backup
contains Secrets (a backup exported with --include-secrets=false prunes none), and only
Secrets managed by Patrol CD (label cd.patrol.example/secret-type or annotation
cd.patrol.example/managed-by: patrol-cd); Secrets that are only referenced are kept.
Pruning never cascades: Applications lose their finalizers first, and objects owned by a
pruned ApplicationSet, Repository, RepositoryCredential, Cluster or SCMProvider are released
from it (owner reference removed) before it is deleted.

Owner references are re-pointed to the UID of the owner in this cluster (owners are
imported first); an owner reference whose owner does not exist is removed, because the
garbage collector would otherwise delete the object.

Objects of another Patrol CD API group (for example a backup taken before the API group
was renamed) are refused unless --migrate-group is given; Argo CD objects (argoproj.io) are
refused: convert them with "patrol admin argocd-import". Operations in a backup (exported
with --include-operations) are never re-created: they would start syncs.

A Cluster's server cannot change. A backup that points a Cluster at another server than the
live Cluster of the same name is refused before anything is written, with or without
--dry-run, listing every such Cluster. A difference of trailing slashes is the same server
and is not refused. With --keep-live-clusters the live Cluster and the Secret it references
stay as they are, also under --prune, and the rest of the backup is restored. A Cluster
skipped by --skip-resources-with-label is not refused, and keeps the Secret it references too,
whether or not the backup holds that Cluster.

A Secret holds one server's credentials. A backup that would start another Cluster reading a
Secret kept this way is refused the same way, unless the backup uses that Secret for the server
of the Cluster it is kept for.

Restoring a Cluster's server (the backup is right, the live Cluster is wrong):
  1. Note the replicas of the ApplicationSet controller, stop it and wait for its pods to go:
     kubectl -n NAMESPACE scale deployment -l app.kubernetes.io/component=applicationset-controller --replicas=0
     kubectl -n NAMESPACE wait pod -l app.kubernetes.io/component=applicationset-controller --for=delete
     Otherwise deleting the Cluster makes ApplicationSets delete its Applications; if the new
     Cluster appears while they do, those Applications keep their finalizer and their
     resources are deleted too.
  2. kubectl -n NAMESPACE delete clusters.cd.patrol.example NAME --cascade=orphan
     Otherwise the credentials Secret the Cluster owns is deleted with it.
  3. If the backup holds no Secrets (--include-secrets=false), put the credentials of the
     backup's server in that Secret. Otherwise the new Cluster uses the old server's.
  4. patrol admin import -n NAMESPACE BACKUP
  5. Scale the ApplicationSet controller back to the replicas noted in step 1.`,
		Example: `# Restore a backup
patrol admin import -n patrol backup.yaml

# Show what would change without writing anything
patrol admin import --dry-run - < backup.yaml

# Restore and delete objects created since the backup
patrol admin import --prune backup.yaml`,
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return runImport(c.Context(), f, kube, args[0], o)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&o.dryRun, "dry-run", false, "Print what will be performed")
	fl.BoolVar(&o.prune, "prune", false, "Prune secrets, applications, projects and the other Patrol CD objects which do not appear in the backup")
	fl.BoolVar(&o.ignoreTracking, "ignore-tracking", false, "Do not update the tracking annotation and label if the resource is already tracked")
	fl.BoolVar(&o.overrideOnConflict, "override-on-conflict", false, "Override the resource on conflict when updating resources")
	fl.BoolVar(&o.verbose, "verbose", false, "Verbose output (versus only changed output)")
	fl.BoolVar(&o.stopOperation, "stop-operation", false, "Stop (cancel) in-flight Operations of the imported Applications")
	fl.StringVar(&o.skipLabel, "skip-resources-with-label", "", "Skip importing resources based on the label e.g. '--skip-resources-with-label my-label/example.io=true'")
	fl.StringSliceVar(&o.appNamespaces, "application-namespaces", nil, fmt.Sprintf(
		"Comma separated list of namespace globs to which import of applications is allowed. If not provided, value from '%s' in %s will be used. If it's not defined, only applications without an explicit namespace will be imported to the control-plane namespace", cmdParamsAppNamespacesKey, settings.DefaultCmdParamsConfigMapName))
	fl.StringSliceVar(&o.appSetNamespaces, "applicationset-namespaces", nil, fmt.Sprintf(
		"Comma separated list of namespace globs which import of applicationsets is allowed. If not provided, value from '%s' in %s will be used. If it's not defined, only applicationsets without an explicit namespace will be imported to the control-plane namespace", cmdParamsAppSetNamespacesKey, settings.DefaultCmdParamsConfigMapName))
	fl.BoolVar(&o.namespaceScoped, "namespace-scoped", false, "Namespace-scoped installation: read and write only the control-plane namespace")
	fl.BoolVar(&o.migrateGroup, "migrate-group", false, fmt.Sprintf("Rewrite Patrol CD objects of another API group (and their %q-style keys) to the installed group %s", currentGroup+"/", currentGroup))
	fl.BoolVar(&o.keepLiveClusters, "keep-live-clusters", false, "When the backup points a Cluster at another server than the live one, keep the live Cluster and its credentials Secret and import the rest")
	fl.BoolVar(&o.promptsEnabled, "prompts-enabled", f.PromptsEnabled, "Force optional interactive prompts to be enabled or disabled, overriding local configuration. If not specified, the local configuration value will be used, which is false by default.")
	return cmd
}

// resourceKey identifies an object by group, kind, namespace and name.
type resourceKey struct {
	Group, Kind, Namespace, Name string
}

func keyOf(obj *unstructured.Unstructured) resourceKey {
	gvk := obj.GroupVersionKind()
	return resourceKey{Group: gvk.Group, Kind: gvk.Kind, Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

// importRank orders backup objects so owners are imported before their dependents.
func importRank(obj *unstructured.Unstructured) int {
	switch obj.GetKind() {
	case "ConfigMap":
		return 0
	case kindAppProject.Kind:
		return 1
	case kindSCMProvider.Kind:
		return 2
	case kindRepository.Kind:
		return 3
	case kindRepositoryCredential.Kind:
		return 4
	case kindCluster.Kind:
		return 5
	case "Secret":
		return 6
	case kindApplicationSet.Kind:
		return 7
	case kindApplication.Kind:
		return 8
	}
	return 9
}

// prepareBackupObjects checks the API groups and kinds of a backup: Patrol objects of
// another group are rewritten with migrateGroup and refused otherwise; Argo CD objects and
// unsupported kinds are refused.
func prepareBackupObjects(objs []*unstructured.Unstructured, migrateGroup bool) ([]*unstructured.Unstructured, error) {
	var problems []string
	foreign := map[string]bool{}
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, obj := range objs {
		g, kind := objectGroup(obj), obj.GetKind()
		switch {
		case g == "" && (kind == "ConfigMap" || kind == "Secret"):
		case isArgoGroup(g):
			problems = append(problems, fmt.Sprintf("%s/%s %s is an Argo CD object; convert Argo CD exports with 'patrol admin argocd-import'", g, kind, obj.GetName()))
			continue
		case g == currentGroup:
			if _, ok := patrolKindByName(kind); !ok {
				problems = append(problems, fmt.Sprintf("%s/%s %s: unsupported kind", g, kind, obj.GetName()))
				continue
			}
		default:
			if _, ok := patrolKindByName(kind); !ok || g == "" {
				problems = append(problems, fmt.Sprintf("%s/%s %s: unsupported kind", g, kind, obj.GetName()))
				continue
			}
			if !migrateGroup {
				foreign[g] = true
				continue
			}
			rewriteObjectGroup(obj, g, currentGroup)
		}
		out = append(out, obj)
	}
	if len(foreign) > 0 {
		problems = append(problems, fmt.Sprintf("the backup contains Patrol CD objects of API group(s) %s but the installed group is %s; re-run with --migrate-group to rewrite them (to copy live objects between groups use 'patrol admin migrate-group')",
			strings.Join(sortedKeys(foreign), ", "), currentGroup))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("refusing to import:\n  %s", strings.Join(problems, "\n  "))
	}
	sort.SliceStable(out, func(i, j int) bool { return importRank(out[i]) < importRank(out[j]) })
	return out, nil
}

// importer applies one backup.
type importer struct {
	f         *Factory
	out       io.Writer
	kc        *kubeClients
	scope     nsScope
	o         importOptions
	dryRunMsg string

	// live are the prune candidates: live objects the backup is expected to describe.
	live map[resourceKey]*unstructured.Unstructured
	// unmanaged are the live Secrets in live that are only referenced (not managed by
	// Patrol CD): restored from a backup, never pruned.
	unmanaged map[resourceKey]bool
	// backupHasSecrets is false for a backup without Secrets (--include-secrets=false):
	// live Secrets are then never pruned.
	backupHasSecrets bool
	// pruned are the objects pruned (or, in a dry run, that would be pruned) so far.
	pruned map[resourceKey]bool
	// dryRunCreated are the objects a dry run would create, with resolved owner references.
	dryRunCreated []*unstructured.Unstructured
	// uids maps objects (live, created or updated) to their UIDs for owner references.
	uids map[resourceKey]types.UID
	// instanceLabel is the label of label-based resource tracking.
	instanceLabel string
	// kept are the backup objects not imported because the live Cluster they belong to points
	// at another server, with the reason printed.
	kept map[resourceKey]string
	// holders holds, by Secret, the live Clusters a Secret in kept stays for.
	holders map[resourceKey][]*unstructured.Unstructured
}

func runImport(ctx context.Context, f *Factory, kube *kubeOptions, source string, o importOptions) error {
	data, err := readSource(source, f.in())
	if err != nil {
		return err
	}
	objs, err := splitObjects(data)
	if err != nil {
		return err
	}
	objs, err = prepareBackupObjects(objs, o.migrateGroup)
	if err != nil {
		return err
	}
	kc, err := kube.get(func(c *rest.Config) { c.QPS, c.Burst = 100, 50 })
	if err != nil {
		return err
	}
	out := f.out()
	_, _ = fmt.Fprintf(out, "import process started %s\n", kc.namespace)
	started := f.now()
	scope, err := resolveScope(ctx, kc.dynamic, kc.namespace, o.appNamespaces, o.appSetNamespaces, o.namespaceScoped, f.logger())
	if err != nil {
		return err
	}
	im := &importer{f: f, out: out, kc: kc, scope: scope, o: o, live: map[resourceKey]*unstructured.Unstructured{}, uids: map[resourceKey]types.UID{},
		unmanaged: map[resourceKey]bool{}, pruned: map[resourceKey]bool{}, backupHasSecrets: containsSecrets(objs), kept: map[resourceKey]string{},
		holders: map[resourceKey][]*unstructured.Unstructured{}}
	if o.dryRun {
		im.dryRunMsg = " (dry run)"
	}
	if err := im.loadLive(ctx, objs); err != nil {
		return err
	}
	if err := im.checkClusterServers(objs); err != nil {
		return err
	}
	var apps []*unstructured.Unstructured
	for _, obj := range objs {
		imported, err := im.importObject(ctx, obj)
		if err != nil {
			return err
		}
		if imported && obj.GetKind() == kindApplication.Kind {
			apps = append(apps, obj)
		}
	}
	if o.stopOperation {
		if err := im.stopOperations(ctx, apps); err != nil {
			return err
		}
	}
	if err := im.pruneRemaining(ctx); err != nil {
		return err
	}
	end := f.now()
	_, _ = fmt.Fprintf(out, "Import process completed successfully in namespace %s at %s, duration: %s\n", kc.namespace, end.Format(time.RFC3339), end.Sub(started))
	return nil
}

func (im *importer) warn(msg string) { im.f.logger().Warn(msg) }

// checkClusterServers compares the Clusters of the backup with the live ones before anything is
// written, because spec.server cannot change. The CRD compares stored text, and every other reader
// compares clusters.NormalizeServer, so a difference of trailing slashes takes the live text. A real
// difference is refused unless the operator chose the live server with --keep-live-clusters. A
// Cluster that stays, by that flag or by the skip label, keeps the Secret it reads, because
// credentials belong to one server. That has to be known before the first Secret is written.
func (im *importer) checkClusterServers(objs []*unstructured.Unstructured) error {
	var moved []string
	inBackup := map[resourceKey]bool{}
	for _, bak := range objs {
		if bak.GetKind() != kindCluster.Kind {
			continue
		}
		if bak.GetNamespace() == "" {
			bak.SetNamespace(im.scope.controlPlane)
		}
		inBackup[keyOf(bak)] = true
		live, ok := im.live[keyOf(bak)]
		if !ok {
			continue
		}
		server, liveServer := serverOf(bak), serverOf(live)
		switch {
		case skipLabelMatches(bak, im.o.skipLabel):
			im.keepCredentials(live)
		case clusters.NormalizeServer(server) == clusters.NormalizeServer(liveServer):
			if err := unstructured.SetNestedField(bak.Object, liveServer, "spec", "server"); err != nil {
				return err
			}
		case im.o.keepLiveClusters:
			im.kept[keyOf(bak)] = serverChange(liveServer, server)
			im.keepCredentials(live)
		default:
			moved = append(moved, bak.GetName()+": "+serverChange(liveServer, server))
		}
	}
	for _, key := range im.liveKeys() {
		if key.Kind == kindCluster.Kind && !inBackup[key] && skipLabelMatches(im.live[key], im.o.skipLabel) {
			im.keepCredentials(im.live[key])
		}
	}
	if len(moved) == 0 {
		return im.checkHeldCredentials(objs)
	}
	return fmt.Errorf(`refusing to import: nothing was written.
The backup points %d Cluster(s) at another server than the live Cluster of the same name, and a Cluster's server cannot change:
  %s
Credentials belong to one server, so choose before importing:
  - If the live server is right, re-run with --keep-live-clusters. The live Cluster and its credentials Secret stay as they are and everything else is restored. Applications using destination.name deploy to the live server; those using the backup's destination.server report InvalidSpecError.
  - If the backup's server is right, do not just delete the Cluster: ApplicationSets would delete its Applications. Follow "Restoring a Cluster's server" in 'patrol admin import --help', then re-run`,
		len(moved), strings.Join(moved, "\n  "))
}

// checkHeldCredentials refuses a Cluster the import would start reading a held Secret with, when the
// live Cluster it is held for is at a server the backup does not use that Secret for: holding it
// gives the new reader that server's credentials, and restoring it would give the backup's to the
// Cluster that stays.
func (im *importer) checkHeldCredentials(objs []*unstructured.Unstructured) error {
	servers := map[resourceKey]map[string]bool{}
	var readers []*unstructured.Unstructured
	for _, bak := range objs {
		secret, ok := credentialsKey(bak)
		if bak.GetKind() != kindCluster.Kind || !ok {
			continue
		}
		if servers[secret] == nil {
			servers[secret] = map[string]bool{}
		}
		servers[secret][clusters.NormalizeServer(serverOf(bak))] = true
		if im.startsReading(bak, secret) {
			readers = append(readers, bak)
		}
	}
	var crossed []string
	for _, bak := range readers {
		secret, _ := credentialsKey(bak)
		for _, holder := range im.holders[secret] {
			if !servers[secret][clusters.NormalizeServer(serverOf(holder))] {
				crossed = append(crossed, fmt.Sprintf("%s: Secret %s is kept for %s at %s", bak.GetName(), secret.Name, holder.GetName(), printableServer(serverOf(holder))))
				break
			}
		}
	}
	if len(crossed) == 0 {
		return nil
	}
	return fmt.Errorf(`refusing to import: nothing was written.
The backup would start %d Cluster(s) reading a Secret kept for a live Cluster at another server:
  %s
Credentials belong to one server, so give the live Cluster its own Secret, or leave the backup's Cluster out with --skip-resources-with-label, then re-run`,
		len(crossed), strings.Join(crossed, "\n  "))
}

// startsReading reports whether the import writes bak, a Cluster of the backup, so that it reads a
// Secret it does not read now.
func (im *importer) startsReading(bak *unstructured.Unstructured, secret resourceKey) bool {
	if _, kept := im.kept[keyOf(bak)]; kept || skipLabelMatches(bak, im.o.skipLabel) {
		return false
	}
	live, ok := im.live[keyOf(bak)]
	if !ok {
		return true
	}
	current, _ := credentialsKey(live)
	return current != secret
}

// keepCredentials holds the Secret a live Cluster reads, so the import neither writes nor prunes it.
func (im *importer) keepCredentials(live *unstructured.Unstructured) {
	if secret, ok := credentialsKey(live); ok {
		im.kept[secret] = "credentials of the live Cluster " + live.GetName()
		im.holders[secret] = append(im.holders[secret], live)
	}
}

// credentialsKey returns the key of the Secret a Cluster reads, if it names one.
func credentialsKey(cluster *unstructured.Unstructured) (resourceKey, bool) {
	name, _, _ := unstructured.NestedString(cluster.Object, "spec", "config", "credentialsRef", "name")
	return resourceKey{Kind: "Secret", Namespace: cluster.GetNamespace(), Name: name}, name != ""
}

func serverOf(cluster *unstructured.Unstructured) string {
	server, _, _ := unstructured.NestedString(cluster.Object, "spec", "server")
	return server
}

// serverChange names both servers of a Cluster for a message.
func serverChange(live, backup string) string {
	l, b := printableServer(live), printableServer(backup)
	if l == b {
		return fmt.Sprintf("live %s, backup %s, which differ only in parts not shown", l, b)
	}
	return fmt.Sprintf("live %s, backup %s", l, b)
}

// printableServer drops what can hold a credential from a server URL for a message: the API
// refuses such a server, the CRD does not.
func printableServer(server string) string {
	u, err := url.Parse(server)
	if err != nil {
		return "(invalid URL)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// containsSecrets reports whether a backup has at least one Secret.
func containsSecrets(objs []*unstructured.Unstructured) bool {
	for _, obj := range objs {
		if obj.GetKind() == "Secret" && objectGroup(obj) == "" {
			return true
		}
	}
	return false
}

// loadLive lists the live objects a backup describes: the settings ConfigMaps, the Patrol
// objects of the enabled namespaces and the Patrol Secrets of the control-plane namespace.
func (im *importer) loadLive(ctx context.Context, objs []*unstructured.Unstructured) error {
	ns := im.scope.controlPlane
	cms, err := im.kc.dynamic.Resource(configMapsGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing configmaps: %w", err)
	}
	referenced := map[string]bool{}
	im.instanceLabel = v1alpha1.LabelKeyAppInstanceDefault
	for i := range cms.Items {
		cm := &cms.Items[i]
		if !isSettingsConfigMap(cm.GetName()) {
			continue
		}
		im.addLive(cm)
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		settingsSecretRefs(data, referenced)
		if cm.GetName() == settings.DefaultConfigMapName && data[settings.KeyInstanceLabelKey] != "" {
			im.instanceLabel = data[settings.KeyInstanceLabelKey]
		}
	}
	for _, k := range configKinds {
		objs, err := listPatrol(ctx, im.kc, im.scope, k, im.warn)
		if err != nil {
			return err
		}
		for _, obj := range objs {
			im.addLive(obj)
			if k != kindApplication && k != kindAppProject && obj.GetNamespace() == ns {
				secretRefs(obj.Object["spec"], referenced)
			}
		}
	}
	// A Secret only a backup Cluster references exists all the same, for one once the Cluster was
	// deleted to restore its server: creating it would fail. TLS Secrets stay out of this: they are
	// the installation's, and a Cluster's credentials are not in that form.
	backupRefs := map[string]bool{}
	for _, obj := range objs {
		if obj.GetKind() == kindCluster.Kind {
			secretRefs(obj.Object["spec"], backupRefs)
		}
	}
	secrets, err := im.kc.dynamic.Resource(secretsGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing secrets: %w", err)
	}
	for i := range secrets.Items {
		sec := &secrets.Items[i]
		secretType, _, _ := unstructured.NestedString(sec.Object, "type")
		if !isPatrolSecret(sec, referenced) && (secretType == string(corev1.SecretTypeTLS) || !isPatrolSecret(sec, backupRefs)) {
			continue
		}
		im.addLive(sec)
		if !isManagedPatrolSecret(sec) {
			im.unmanaged[keyOf(sec)] = true
		}
	}
	return nil
}

// liveKeys returns the keys of the live objects in a stable order.
func (im *importer) liveKeys() []resourceKey {
	keys := slices.Collect(maps.Keys(im.live))
	slices.SortFunc(keys, func(a, b resourceKey) int {
		return strings.Compare(a.Kind+"/"+a.Namespace+"/"+a.Name, b.Kind+"/"+b.Namespace+"/"+b.Name)
	})
	return keys
}

func (im *importer) addLive(obj *unstructured.Unstructured) {
	k := keyOf(obj)
	im.live[k] = obj
	im.uids[k] = obj.GetUID()
}

// resource returns the dynamic client of obj's kind in its namespace.
func (im *importer) resource(obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	return resourceFor(im.kc.dynamic, obj.GroupVersionKind().GroupKind(), obj.GetNamespace())
}

// resourceFor returns the dynamic client of a ConfigMap, Secret or Patrol kind.
func resourceFor(dyn dynamic.Interface, gk schema.GroupKind, namespace string) (dynamic.ResourceInterface, error) {
	switch {
	case gk.Group == "" && gk.Kind == "ConfigMap":
		return dyn.Resource(configMapsGVR).Namespace(namespace), nil
	case gk.Group == "" && gk.Kind == "Secret":
		return dyn.Resource(secretsGVR).Namespace(namespace), nil
	}
	k, ok := patrolKindByName(gk.Kind)
	if !ok || gk.Group == "" {
		return nil, fmt.Errorf("unsupported kind %s", gk)
	}
	return dyn.Resource(k.gvr(gk.Group)).Namespace(namespace), nil
}

// skipLabelMatches reports whether obj carries the --skip-resources-with-label label
// (upstream isSkipLabelMatches).
func skipLabelMatches(obj *unstructured.Unstructured, skip string) bool {
	if skip == "" {
		return false
	}
	key, value, ok := strings.Cut(skip, "=")
	if !ok || key == "" || value == "" {
		return false
	}
	v, found := obj.GetLabels()[key]
	return found && v == value
}

// importObject creates or updates one backup object. It reports whether the object was
// imported (not skipped).
func (im *importer) importObject(ctx context.Context, bak *unstructured.Unstructured) (bool, error) {
	if bak.GetNamespace() == "" {
		bak.SetNamespace(im.scope.controlPlane)
	}
	gvk := bak.GroupVersionKind()
	key := keyOf(bak)
	liveObj, exists := im.live[key]
	delete(im.live, key)

	if skipLabelMatches(bak, im.o.skipLabel) {
		_, _ = fmt.Fprintf(im.out, "Skipping %s/%s %s in namespace %s\n", gvk.Group, gvk.Kind, bak.GetName(), bak.GetNamespace())
		return false, nil
	}
	if reason, ok := im.kept[key]; ok {
		outcome := "kept"
		if !exists {
			outcome = "not restored"
		}
		_, _ = fmt.Fprintf(im.out, "%s/%s %s in namespace %s %s: %s%s\n", gvk.Group, gvk.Kind, bak.GetName(), bak.GetNamespace(), outcome, reason, im.dryRunMsg)
		return false, nil
	}
	switch kind := bak.GetKind(); {
	case kind == kindOperation.Kind:
		_, _ = fmt.Fprintf(im.out, "Skipping %s/%s %s in namespace %s: Operations are sync records and are never re-created (importing one would start a sync)\n", gvk.Group, gvk.Kind, bak.GetName(), bak.GetNamespace())
		return false, nil
	case kind == "ConfigMap" && !isSettingsConfigMap(bak.GetName()):
		_, _ = fmt.Fprintf(im.out, "Skipping /ConfigMap %s in namespace %s: not a Patrol CD settings ConfigMap\n", bak.GetName(), bak.GetNamespace())
		return false, nil
	}
	if k, ok := patrolKindByName(bak.GetKind()); ok && !im.scope.enabled(k, bak.GetNamespace()) {
		return false, nil
	}
	if bak.GetNamespace() != im.scope.controlPlane {
		k, _ := patrolKindByName(bak.GetKind())
		if !k.AppScoped {
			im.warn(fmt.Sprintf("%s/%s %s: namespace %s is not the control-plane namespace %s; skipped", gvk.Group, gvk.Kind, bak.GetName(), bak.GetNamespace(), im.scope.controlPlane))
			return false, nil
		}
	}
	if im.o.ignoreTracking && exists {
		keepTracking(bak, liveObj, im.instanceLabel)
	}
	if err := im.resolveOwners(ctx, bak); err != nil {
		return false, err
	}
	dc, err := im.resource(bak)
	if err != nil {
		return false, err
	}
	switch {
	case !exists:
		forbidden := false
		if im.o.dryRun {
			if _, ok := im.uids[key]; !ok {
				im.uids[key] = "" // would be created: owners of later objects resolve in dry-run
			}
			im.dryRunCreated = append(im.dryRunCreated, bak)
		} else {
			created, err := dc.Create(ctx, bak, metav1.CreateOptions{})
			switch {
			case apierrors.IsForbidden(err) || apierrors.IsNotFound(err):
				forbidden = true
				im.warn(fmt.Sprintf("%s/%s %s: %v", gvk.Group, gvk.Kind, bak.GetName(), err))
			case err != nil:
				return false, fmt.Errorf("error creating %s/%s %s: %w", gvk.Group, gvk.Kind, bak.GetName(), err)
			default:
				im.uids[key] = created.GetUID()
			}
		}
		if !forbidden {
			_, _ = fmt.Fprintf(im.out, "%s/%s %s in namespace %s created%s\n", gvk.Group, gvk.Kind, bak.GetName(), bak.GetNamespace(), im.dryRunMsg)
		}
	case specsEqual(bak, liveObj) && hasItsOwners(bak, liveObj):
		if im.o.verbose {
			_, _ = fmt.Fprintf(im.out, "%s/%s %s unchanged%s\n", gvk.Group, gvk.Kind, bak.GetName(), im.dryRunMsg)
		}
	default:
		forbidden := false
		if !im.o.dryRun {
			newLive := updateLive(bak, liveObj)
			updated, err := dc.Update(ctx, newLive, metav1.UpdateOptions{})
			if apierrors.IsConflict(err) {
				_, _ = fmt.Fprintf(im.out, "Failed to update %s/%s %s in namespace %s: %v\n", gvk.Group, gvk.Kind, bak.GetName(), bak.GetNamespace(), err)
				if im.o.overrideOnConflict {
					err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
						_, _ = fmt.Fprintf(im.out, "Resource conflict: retrying update for Group: %s, Kind: %s, Name: %s, Namespace: %s\n", gvk.Group, gvk.Kind, bak.GetName(), bak.GetNamespace())
						cur, getErr := dc.Get(ctx, bak.GetName(), metav1.GetOptions{})
						if getErr != nil {
							return getErr
						}
						newLive = updateLive(bak, cur)
						var uerr error
						updated, uerr = dc.Update(ctx, newLive, metav1.UpdateOptions{})
						return uerr
					})
				}
			}
			switch {
			case apierrors.IsForbidden(err) || apierrors.IsNotFound(err):
				forbidden = true
				im.warn(fmt.Sprintf("%s/%s %s: %v", gvk.Group, gvk.Kind, bak.GetName(), err))
			case err != nil:
				return false, fmt.Errorf("error updating %s/%s %s: %w", gvk.Group, gvk.Kind, bak.GetName(), err)
			default:
				im.uids[key] = updated.GetUID()
			}
		}
		if !forbidden {
			_, _ = fmt.Fprintf(im.out, "%s/%s %s in namespace %s updated%s\n", gvk.Group, gvk.Kind, bak.GetName(), bak.GetNamespace(), im.dryRunMsg)
		}
	}
	return true, nil
}

// resolveOwners points the owner references of bak at the owners' UIDs in this cluster.
// A reference whose owner cannot be found is removed: a dangling owner reference makes the
// garbage collector delete the object. In dry-run mode owners that the backup creates
// earlier are kept.
func (im *importer) resolveOwners(ctx context.Context, bak *unstructured.Unstructured) error {
	refs := bak.GetOwnerReferences()
	if len(refs) == 0 {
		return nil
	}
	kept := refs[:0]
	for _, ref := range refs {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			im.warn(fmt.Sprintf("%s: owner reference with invalid apiVersion %q removed", objectRef(bak), ref.APIVersion))
			continue
		}
		key := resourceKey{Group: gv.Group, Kind: ref.Kind, Namespace: bak.GetNamespace(), Name: ref.Name}
		if uid, ok := im.uids[key]; ok && uid != "" {
			ref.UID = uid
			kept = append(kept, ref)
			continue
		}
		if im.o.dryRun {
			if _, ok := im.uids[key]; ok {
				kept = append(kept, ref)
				continue
			}
		}
		dc, err := resourceFor(im.kc.dynamic, schema.GroupKind{Group: gv.Group, Kind: ref.Kind}, bak.GetNamespace())
		if err == nil {
			owner, gerr := dc.Get(ctx, ref.Name, metav1.GetOptions{})
			switch {
			case gerr == nil:
				im.uids[key] = owner.GetUID()
				ref.UID = owner.GetUID()
				kept = append(kept, ref)
				continue
			case !apierrors.IsNotFound(gerr) && !apierrors.IsForbidden(gerr):
				return fmt.Errorf("error getting owner %s %s of %s: %w", ref.Kind, ref.Name, objectRef(bak), gerr)
			}
		}
		if im.o.dryRun {
			// The owner may be part of the backup but skipped; report and drop.
			im.warn(fmt.Sprintf("%s: owner %s %s not found; the owner reference would be removed", objectRef(bak), ref.Kind, ref.Name))
			continue
		}
		im.warn(fmt.Sprintf("%s: owner %s %s not found; owner reference removed (a dangling owner reference would make the garbage collector delete the object)", objectRef(bak), ref.Kind, ref.Name))
	}
	if len(kept) == 0 {
		bak.SetOwnerReferences(nil)
	} else {
		bak.SetOwnerReferences(kept)
	}
	return nil
}

// keepTracking copies the live values of the tracking annotation and instance label into
// bak when both carry them (upstream updateTracking).
func keepTracking(bak, live *unstructured.Unstructured, instanceLabel string) {
	if la, ba := live.GetAnnotations(), bak.GetAnnotations(); la != nil && ba != nil {
		if v, ok := la[v1alpha1.AnnotationKeyTrackingID]; ok {
			if _, ok := ba[v1alpha1.AnnotationKeyTrackingID]; ok {
				ba[v1alpha1.AnnotationKeyTrackingID] = v
				bak.SetAnnotations(ba)
			}
		}
	}
	if ll, bl := live.GetLabels(), bak.GetLabels(); ll != nil && bl != nil {
		if v, ok := ll[instanceLabel]; ok {
			if _, ok := bl[instanceLabel]; ok {
				bl[instanceLabel] = v
				bak.SetLabels(bl)
			}
		}
	}
}

// jsonEqual compares two JSON-like values by their canonical JSON encoding, so int64 and
// float64 numbers of the same value are equal and nil equals an empty map.
func jsonEqual(a, b any) bool {
	norm := func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			if len(t) == 0 {
				return nil
			}
		case map[string]string:
			if len(t) == 0 {
				return nil
			}
		case []string:
			if len(t) == 0 {
				return nil
			}
		}
		return v
	}
	ja, err1 := json.Marshal(norm(a))
	jb, err2 := json.Marshal(norm(b))
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

func annotationsWithoutLastApplied(obj *unstructured.Unstructured) map[string]string {
	a := maps.Clone(obj.GetAnnotations())
	delete(a, corev1.LastAppliedConfigAnnotation)
	return a
}

// specsEqual reports whether the backup object needs no update: labels, annotations
// (ignoring last-applied-configuration), finalizers and data (ConfigMap, Secret) or spec
// are equal (upstream specsEqual; Patrol status is a subresource and never compared).
func specsEqual(bak, live *unstructured.Unstructured) bool {
	if !jsonEqual(annotationsWithoutLastApplied(bak), annotationsWithoutLastApplied(live)) ||
		!jsonEqual(bak.GetLabels(), live.GetLabels()) ||
		!jsonEqual(bak.GetFinalizers(), live.GetFinalizers()) {
		return false
	}
	switch bak.GetKind() {
	case "ConfigMap":
		return jsonEqual(bak.Object["data"], live.Object["data"]) && jsonEqual(bak.Object["binaryData"], live.Object["binaryData"])
	case "Secret":
		return jsonEqual(bak.Object["data"], live.Object["data"])
	default:
		return jsonEqual(bak.Object["spec"], live.Object["spec"])
	}
}

// hasItsOwners reports whether an object has the owners the backup gives it, resolved to this
// cluster. The steps in import's help orphan a Secret that may match the backup, and without an owner
// it would outlive the new Cluster.
func hasItsOwners(bak, live *unstructured.Unstructured) bool {
	refs := bak.GetOwnerReferences()
	return len(refs) == 0 || jsonEqual(refs, live.GetOwnerReferences())
}

// updateLive returns live with the labels, annotations, finalizers, owner references and
// data or spec of bak; status and other metadata stay as they are.
func updateLive(bak, live *unstructured.Unstructured) *unstructured.Unstructured {
	newLive := live.DeepCopy()
	newLive.SetAnnotations(bak.GetAnnotations())
	newLive.SetLabels(bak.GetLabels())
	newLive.SetFinalizers(bak.GetFinalizers())
	if refs := bak.GetOwnerReferences(); len(refs) > 0 {
		newLive.SetOwnerReferences(refs)
	}
	switch live.GetKind() {
	case "ConfigMap":
		setOrDelete(newLive.Object, "data", bak.Object["data"])
		setOrDelete(newLive.Object, "binaryData", bak.Object["binaryData"])
	case "Secret":
		setOrDelete(newLive.Object, "data", bak.Object["data"])
	default:
		setOrDelete(newLive.Object, "spec", bak.Object["spec"])
	}
	return newLive
}

func setOrDelete(m map[string]any, key string, v any) {
	if v == nil {
		delete(m, key)
		return
	}
	m[key] = v
}

// stopOperations requests cancellation of the in-flight Operations of the imported
// Applications (the Patrol equivalent of upstream clearing Application.operation).
func (im *importer) stopOperations(ctx context.Context, apps []*unstructured.Unstructured) error {
	byNS := map[string]map[string]bool{}
	for _, app := range apps {
		if byNS[app.GetNamespace()] == nil {
			byNS[app.GetNamespace()] = map[string]bool{}
		}
		byNS[app.GetNamespace()][app.GetName()] = true
	}
	for _, ns := range sortedKeys(byNS) {
		dc := im.kc.dynamic.Resource(kindOperation.gvr(currentGroup)).Namespace(ns)
		list, err := dc.List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				im.warn(fmt.Sprintf("%s/Operation: %v", currentGroup, err))
				continue
			}
			return fmt.Errorf("error listing operations: %w", err)
		}
		for i := range list.Items {
			op := &list.Items[i]
			app, _, _ := unstructured.NestedString(op.Object, "spec", "application")
			phase, _, _ := unstructured.NestedString(op.Object, "status", "phase")
			cancel, _, _ := unstructured.NestedBool(op.Object, "spec", "cancel")
			if !byNS[ns][app] || v1alpha1.OperationPhase(phase).IsTerminal() || cancel {
				continue
			}
			if !im.o.dryRun {
				patch := []byte(`{"spec":{"cancel":true}}`)
				if _, err := dc.Patch(ctx, op.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("error stopping operation %s: %w", op.GetName(), err)
				}
			}
			_, _ = fmt.Fprintf(im.out, "%s/Operation %s of application %s in namespace %s stopped%s\n", currentGroup, op.GetName(), app, ns, im.dryRunMsg)
		}
	}
	return nil
}

// pruneRemaining deletes (with --prune) or reports the live objects not in the backup.
func (im *importer) pruneRemaining(ctx context.Context) error {
	var prompt *bufio.Reader
	for _, key := range im.liveKeys() {
		liveObj := im.live[key]
		if skipLabelMatches(liveObj, im.o.skipLabel) {
			_, _ = fmt.Fprintf(im.out, "Skipping pruning of %s/%s %s in namespace %s\n", key.Group, key.Kind, key.Name, key.Namespace)
			continue
		}
		if key.Kind == "ConfigMap" {
			_, _ = fmt.Fprintf(im.out, "%s/%s %s is not in the backup; settings ConfigMaps are never pruned\n", key.Group, key.Kind, key.Name)
			continue
		}
		if reason := im.secretKeptReason(key); reason != "" {
			_, _ = fmt.Fprintf(im.out, "%s/%s %s is not in the backup; not pruned: %s\n", key.Group, key.Kind, key.Name, reason)
			continue
		}
		if !im.o.prune {
			_, _ = fmt.Fprintf(im.out, "%s/%s %s needs pruning\n", key.Group, key.Kind, key.Name)
			continue
		}
		dc, err := resourceFor(im.kc.dynamic, schema.GroupKind{Group: key.Group, Kind: key.Kind}, key.Namespace)
		if err != nil {
			return err
		}
		if im.o.dryRun {
			if _, err := im.releaseDependents(ctx, key, liveObj); err != nil {
				return err
			}
			im.pruned[key] = true
			_, _ = fmt.Fprintf(im.out, "%s/%s %s pruned%s\n", key.Group, key.Kind, key.Name, im.dryRunMsg)
			continue
		}
		if im.o.promptsEnabled {
			if prompt == nil {
				prompt = bufio.NewReader(im.f.in())
			}
			if !confirm(prompt, im.out, fmt.Sprintf("Are you sure you want to prune %s/%s %s ? [y/n]", key.Group, key.Kind, key.Name)) {
				_, _ = fmt.Fprintf(im.out, "The command to prune %s/%s %s was cancelled.\n", key.Group, key.Kind, key.Name)
				continue
			}
		}
		if key.Kind == kindApplication.Kind && len(liveObj.GetFinalizers()) > 0 {
			// Never cascade: pruning an Application from the configuration must not
			// delete the resources it deployed (upstream removes the finalizers first).
			if _, err := dc.Patch(ctx, key.Name, types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("error removing finalizers of %s/%s %s: %w", key.Group, key.Kind, key.Name, err)
			}
		}
		released, err := im.releaseDependents(ctx, key, liveObj)
		if err != nil {
			return err
		}
		if !released {
			continue
		}
		err = dc.Delete(ctx, key.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: ptr(liveObj.GetUID())}})
		switch {
		case apierrors.IsForbidden(err) || apierrors.IsNotFound(err) || apierrors.IsConflict(err):
			im.warn(fmt.Sprintf("%s/%s %s: %v", key.Group, key.Kind, key.Name, err))
			continue
		case err != nil:
			return fmt.Errorf("error pruning %s/%s %s: %w", key.Group, key.Kind, key.Name, err)
		}
		im.pruned[key] = true
		_, _ = fmt.Fprintf(im.out, "%s/%s %s pruned%s\n", key.Group, key.Kind, key.Name, im.dryRunMsg)
	}
	return nil
}

// secretKeptReason returns why a live Secret not in the backup is not pruned, or "".
func (im *importer) secretKeptReason(key resourceKey) string {
	if key.Group != "" || key.Kind != "Secret" {
		return ""
	}
	switch {
	case im.kept[key] != "":
		return im.kept[key]
	case isRequiredSettingsSecret(key.Name):
		return fmt.Sprintf("%s and %s are never pruned", settings.DefaultSecretName, settings.DefaultNotificationsSecretName)
	case !im.backupHasSecrets:
		return "the backup contains no Secrets (exported with --include-secrets=false?)"
	case im.unmanaged[key]:
		return fmt.Sprintf("not managed by Patrol CD (no %s label or %s: %s annotation)", v1alpha1.LabelKeySecretType, v1alpha1.AnnotationKeyManagedBy, v1alpha1.AnnotationValueManagedByPatrol)
	}
	return ""
}

func ptr[T any](v T) *T { return &v }

// dependentsOf returns the client of the kind that objects of key's kind own in a Patrol
// configuration (ApplicationSets own their generated Applications; Repositories,
// RepositoryCredentials, Clusters and SCMProviders own their credentials Secrets) and its
// printed name, or nil.
func (im *importer) dependentsOf(key resourceKey) (dynamic.ResourceInterface, string, string) {
	switch key.Kind {
	case kindApplicationSet.Kind:
		return im.kc.dynamic.Resource(kindApplication.gvr(key.Group)).Namespace(key.Namespace), key.Group, kindApplication.Kind
	case kindRepository.Kind, kindRepositoryCredential.Kind, kindCluster.Kind, kindSCMProvider.Kind:
		return im.kc.dynamic.Resource(secretsGVR).Namespace(key.Namespace), "", "Secret"
	}
	return nil, "", ""
}

// withoutOwner returns refs without the references to uid.
func withoutOwner(refs []metav1.OwnerReference, uid types.UID) []metav1.OwnerReference {
	out := slices.DeleteFunc(slices.Clone(refs), func(r metav1.OwnerReference) bool { return r.UID == uid })
	if len(out) == 0 {
		return nil
	}
	return out
}

// releaseDependents removes the owner references to owner from the objects it owns before
// owner is pruned, so that neither the garbage collector nor the ApplicationSet controller's
// ordered deletion deletes them: objects of the backup and objects skipped by label must
// survive, and objects not in the backup are pruned on their own (Applications without
// their finalizers). Upstream deletes the owner and lets its dependents cascade. It reports
// false when the dependents cannot be listed: the owner is then not pruned.
func (im *importer) releaseDependents(ctx context.Context, key resourceKey, owner *unstructured.Unstructured) (bool, error) {
	dc, group, kind := im.dependentsOf(key)
	if dc == nil {
		return true, nil
	}
	uid := owner.GetUID()
	list, err := dc.List(ctx, metav1.ListOptions{})
	switch {
	case apierrors.IsForbidden(err) || apierrors.IsNotFound(err):
		im.warn(fmt.Sprintf("%s/%s %s not pruned: cannot list the %s objects it may own: %v", key.Group, key.Kind, key.Name, kind, err))
		return false, nil
	case err != nil:
		return false, fmt.Errorf("error listing the objects owned by %s/%s %s: %w", key.Group, key.Kind, key.Name, err)
	}
	report := func(name string) {
		_, _ = fmt.Fprintf(im.out, "%s/%s %s: owner reference to %s %s removed before pruning the owner%s\n", group, kind, name, key.Kind, key.Name, im.dryRunMsg)
	}
	seen := map[string]bool{}
	for i := range list.Items {
		dep := &list.Items[i]
		depKey := keyOf(dep)
		if im.pruned[depKey] || dep.GetDeletionTimestamp() != nil || len(withoutOwner(dep.GetOwnerReferences(), uid)) == len(dep.GetOwnerReferences()) {
			continue
		}
		seen[dep.GetName()] = true
		if !im.o.dryRun {
			cur := dep.DeepCopy()
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				cur.SetOwnerReferences(withoutOwner(cur.GetOwnerReferences(), uid))
				_, uerr := dc.Update(ctx, cur, metav1.UpdateOptions{})
				if apierrors.IsConflict(uerr) {
					fresh, gerr := dc.Get(ctx, cur.GetName(), metav1.GetOptions{})
					if gerr != nil {
						return gerr
					}
					cur = fresh
				}
				return uerr
			})
			switch {
			case apierrors.IsNotFound(err):
				continue
			case err != nil:
				return false, fmt.Errorf("error releasing %s/%s %s from %s %s: %w", group, kind, dep.GetName(), key.Kind, key.Name, err)
			}
		}
		report(dep.GetName())
	}
	if im.o.dryRun {
		// Objects the dry run would create are not live yet.
		for _, dep := range im.dryRunCreated {
			if dep.GetKind() != kind || objectGroup(dep) != group || dep.GetNamespace() != key.Namespace || seen[dep.GetName()] {
				continue
			}
			if len(withoutOwner(dep.GetOwnerReferences(), uid)) != len(dep.GetOwnerReferences()) {
				report(dep.GetName())
			}
		}
	}
	return true, nil
}

// confirm asks a yes/no question; anything but y/yes is no.
func confirm(in *bufio.Reader, out io.Writer, question string) bool {
	_, _ = fmt.Fprintln(out, question)
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
