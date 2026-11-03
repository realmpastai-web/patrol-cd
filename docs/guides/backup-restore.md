# Backup, restore and disaster recovery

`patrol admin export` writes the configuration of an installation to a YAML stream, and
`patrol admin import` writes it back. Both talk to the Kubernetes API with your kubeconfig, not
to patrol-server, so they work while Patrol CD is down. Upstream counterpart: Argo CD's
`argocd admin export` and `import` and its disaster recovery page (OPS-142 to OPS-144).

What a restore brings back is the configuration: projects, repositories and their credentials,
clusters and their credentials, Applications, ApplicationSets, settings, the admin password and
the session signing key. It does not bring back the workloads Patrol deployed; it does not need
to, because a restore never touches the destination clusters. Restored Applications find their
resources by name and report them Synced without syncing again.

Status: `make kind-restore-drill` (`hack/kind/restore-drill.sh`) runs the procedure below on the
local kind clusters ([local-development.md](local-development.md)), with the Helm chart's
default profile and the image built from the same commit. It exports a populated installation,
removes it, installs the same chart version with the same values, imports, and checks that the
7 fixture Applications come back Synced/Healthy without a sync, that their workloads on the
destination cluster keep their UIDs and generations, that the old admin password works and the
new installation's does not, and that a restored repository credential passes the connection
check. Two losses are drilled: the Patrol namespace (the default, and a step of the CI kind job)
and the whole control-plane cluster (`RESTORE_DRILL_FLAGS=--new-cluster`). Results:
[run record](../verification/runs/2026-11-03-restore-drill.md), which also has the
[scheduled backup](#scheduled-backups) and the
[wrong-namespace cases](#exporting-or-importing-in-the-wrong-namespace), each run once by hand on
the same clusters. Not run: the static manifests, the namespace-scoped and core variants, HA,
Applications in other namespaces, ApplicationSets, project-role tokens, SSO, and a restore into
another namespace, release name or version.

## What a backup holds

| In the backup | Not in the backup |
|---------------|-------------------|
| The settings ConfigMaps `patrol-cm`, `patrol-rbac-cm`, `patrol-ssh-known-hosts-cm`, `patrol-tls-certs-cm` (required: export fails without them), `patrol-gpg-keys-cm` and `patrol-notifications-cm` (when present) | `patrol-cmd-params-cm`: it comes from your Helm values or overlay, so keep those with the backup ([Version pinning](#version-pinning)) |
| `patrol-secret` (session signing key, admin and local account passwords and tokens, webhook secrets, SSO client secrets referenced from settings) and `patrol-notifications-secret` | `patrol-internal-token`, `patrol-initial-admin-secret`, ServiceAccount token Secrets and Helm release Secrets ([What is regenerated](#what-is-regenerated)) |
| Secrets labelled `cd.patrol.example/secret-type` or annotated `cd.patrol.example/managed-by: patrol-cd` (what the API and the CLI create), and Secrets referenced from the specs of Repositories, RepositoryCredentials, Clusters, SCMProviders and ApplicationSets, or as `$secret:key` from settings | `patrol-session-revocations`: logouts are forgotten ([After the import](#after-the-import)) |
| AppProjects, SCMProviders, Repositories, RepositoryCredentials, Clusters, ApplicationSets and Applications of the Patrol namespace, plus Applications and ApplicationSets of the enabled additional namespaces | TLS Secrets you provide (`server.tls.existingSecret`, internal TLS `existingSecret`) or cert-manager issues: nothing labels them; keep your own copy or let cert-manager issue them again |
| Labels, annotations (without `kubectl.kubernetes.io/last-applied-configuration`), finalizers and owner references (UIDs cleared) of every object | `status` of every object: sync and health are computed again, but sync history, the last operation and project token status are gone; rollback to an earlier history ID is not possible after a restore |
| Operations, with their status, only with `--include-operations` (a record; import never re-creates them, because that would start syncs) | The CRDs and every other object of the installation, which the install creates; Dex keeps nothing (memory storage, no volumes) |

## Version pinning

A backup carries no version, and import writes `cd.patrol.example/v1alpha1` objects as they are.
Restore with the CLI of the installed version onto an installation of the same version, and
upgrade afterwards. Keep with every backup:

- the chart version and image tag:

  ```bash
  helm list -n patrol --filter '^patrol$'          # CHART and APP VERSION columns
  kubectl -n patrol get deployment patrol-server -o jsonpath='{.spec.template.spec.containers[0].image}'
  ```

- the Helm values (`helm get values patrol -n patrol -o yaml`), or your kustomize overlay for a
  static installation. They may hold secrets too;
- copies of the TLS Secrets you provide.

The image ships the `patrol` CLI, which is the simplest way to run the matching version. The
container cannot reach a kubeconfig server address of `127.0.0.1`; use one it can reach (the drill
uses `kind get kubeconfig --internal` with `--network kind`):

```bash
mkdir -m 700 backups
docker run --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$HOME/.kube/config:/tmp/kubeconfig:ro" -v "$PWD/backups:/backups" \
  registry.example.com/patrol-cd/patrol-cd:0.1.0 \
  patrol admin export --kubeconfig /tmp/kubeconfig -n patrol -o /backups/patrol-backup.yaml
```

Take a backup before every upgrade. Before 1.0.0 a minor version may break compatibility
([releasing.md](releasing.md)).

## Namespace and scope

Always pass `-n NAMESPACE` to both commands. Without it they use the namespace of the kubeconfig
context, or `default` when the context sets none. Select the cluster with `--context`: the
`patrol` command's global `--kube-context` flag is not read by `admin` (#288).

Applications and ApplicationSets outside the Patrol namespace (apps in any namespace) are
included when their namespace matches the globs in `patrol-cmd-params-cm`
(`application.namespaces` and `applicationsetcontroller.namespaces`, set from the chart's
`applicationNamespaces` and `applicationSetNamespaces`), or the globs given with
`--application-namespaces` and `--applicationset-namespaces`. With globs, Applications and
ApplicationSets are listed cluster-wide and then filtered, so the account needs to list them in
every namespace.

| Flag (export and import) | Effect |
|------|--------|
| `--application-namespaces`, `--applicationset-namespaces` | Globs used instead of those in `patrol-cmd-params-cm` |
| `--namespace-scoped` | Reads and writes only the Patrol namespace, with no cluster-wide list; for namespace-scoped installations. Cannot be combined with the glob flags |

On import, an Application or ApplicationSet in a namespace the scope does not enable is skipped
without a message, and one whose namespace does not exist is a warning. Restore the namespace
settings (the same Helm values) and create the namespaces before the import. No test exercises
these flags yet.

## Export

```bash
patrol admin export -n patrol -o patrol-backup.yaml
```

- `-o` creates the file with mode `0600` and writes it only after everything was read, so a failed
  export leaves no file. A shell redirect follows your umask and leaves an empty file behind when
  the export fails.
- `WARNING: the export contains N Secret(s) with credentials …` on stderr is expected: Secrets are
  included by default. `--include-secrets=false` leaves them out; a restore of such a backup then
  needs every credential entered again, and `import --prune` of it removes no Secret.
- Exit status 0 does not prove the backup is complete. A list of a Patrol kind refused with
  `Forbidden` (RBAC) or `NotFound` (a missing CRD) is a `level=WARN` line on stderr, and that kind
  is left out. With namespace globs, a refused cluster-wide list loses every Application, those of
  the Patrol namespace too. Treat any `level=WARN` line as a failed backup, and compare counts:

  ```bash
  grep -c '^kind: Application$' patrol-backup.yaml
  kubectl -n patrol get applications.cd.patrol.example --no-headers | wc -l
  ```

### Handling the backup file

Base64 is not encryption. A backup with Secrets holds the session signing key (whoever has it can
sign sessions Patrol accepts, as any user), the admin password hash, every repository and cluster
credential, webhook secrets and SSO client secrets. Store it like the most sensitive credential of
the installation: encrypted at rest (for example with `age`, `sops` or a KMS-encrypted bucket),
readable by the people who run restores only, never in Git, a CI artifact or a log, and deleted
when your retention period ends.

## What is regenerated

| Object | On a new installation | After the import |
|--------|-----------------------|------------------|
| `patrol-internal-token` (component-to-component token) | Created by the bootstrap | Unchanged: the backup does not hold it |
| `patrol-initial-admin-secret` | Created by the bootstrap with a new password | Stale: the import restores the old `admin.password`, and the bootstrap never changes an existing one. Delete it |
| `server.secretkey` in `patrol-secret` | Generated | Replaced by the backup's key; patrol-server takes it up without a restart |
| `patrol-session-revocations` | Absent until the first logout | Still absent: revoked sessions are valid again until they expire |
| The `default` AppProject | Created by patrol-server (not in a core installation) | Updated to the backup's spec |
| Application status | Computed by the controller | Computed by the controller; history is empty |
| Self-signed server and internal TLS certificates | Generated in memory | Unchanged; `tls.crt`/`tls.key` in `patrol-secret`, when used, come back with it |

## Restore

### Remove a damaged installation without touching workloads

Skip this when the namespace or the cluster is already gone. To remove an installation you are
about to restore, stop its controllers first and remove the finalizers of its Applications,
ApplicationSets and AppProjects. A running controller deletes an Application that carries a
deletion finalizer the way a user's cascading delete would: it runs its PreDelete hooks, deletes
its resources on the destination cluster, then runs its PostDelete hooks.

1. Export, if the installation still answers.
2. Let running syncs finish, so no hook is left half way. List what is still running:

   ```bash
   kubectl -n patrol get operations.cd.patrol.example \
     -o custom-columns=NAME:.metadata.name,PHASE:.status.phase
   ```

   `Succeeded`, `Failed`, `Error`, `Cancelled` and `Rejected` are final. Any other phase, or an
   empty one, is a sync in progress or about to start.
3. Stop the application controller and the ApplicationSet controller, and wait for their pods to
   go (the chart's `controller.replicas` cannot be 0; scaling the release you are about to delete
   is harmless):

   ```bash
   kubectl -n patrol scale statefulset/patrol-controller deployment/patrol-applicationset-controller --replicas=0
   kubectl -n patrol wait pod --for=delete --timeout=5m \
     -l 'app.kubernetes.io/name in (patrol-controller,patrol-applicationset-controller)'
   ```

4. Remove the finalizers. A controller that still runs adds some of them back, so do this after
   step 3:

   ```bash
   for o in $(kubectl -n patrol get applications.cd.patrol.example,applicationsets.cd.patrol.example,appprojects.cd.patrol.example -o name); do
     kubectl -n patrol patch "$o" --type=merge -p '{"metadata":{"finalizers":null}}'
   done
   ```

5. Delete the namespace and wait until it is gone; Kubernetes refuses new objects in a namespace
   that is still terminating:

   ```bash
   kubectl delete namespace patrol --wait=false
   kubectl wait --for=delete namespace/patrol --timeout=10m
   ```

Never remove an installation with `kubectl delete -f install.yaml` (or `core-install.yaml`,
`ha/install.yaml`): those files hold the CRDs, and deleting a CRD deletes every object of that
kind in the cluster. Applications and ApplicationSets in other namespaces survive the namespace
deletion. The new installation reconciles them before the import restores their projects,
clusters and credentials, and they report errors until it does.

### Install the same version

Install with the chart version and values of the backup (with the static manifests: the same file
and overlay), and wait until it is ready:

```bash
helm install patrol deploy/helm/patrol-cd -n patrol --create-namespace \
  -f patrol-values.yaml --wait --timeout 10m
kubectl -n patrol wait appprojects.cd.patrol.example/default --for=create --timeout=5m
```

Install from a checkout of the backup's release tag, or from that version's packaged chart
(`patrol-cd-0.1.0.tgz`): Helm ignores `--version` for a chart directory. After a namespace loss
the CRDs are still there (the chart keeps them): Helm adopts them, as the drill shows. Create the
TLS Secrets you provide before the install.

Wait for the `default` AppProject: patrol-server reports ready just before it creates it, and an
import that creates `default` first stops with `already exists`. Re-running the import is safe
when that happens; it is idempotent.

### Import

If the backup holds ApplicationSets, stop the ApplicationSet controller for the import. Otherwise it
can generate an Application between the import's ApplicationSet and Application writes, and the
import stops with `already exists` (re-running it then completes):

```bash
kubectl -n patrol scale deployment/patrol-applicationset-controller --replicas=0
patrol admin import -n patrol patrol-backup.yaml
kubectl -n patrol scale deployment/patrol-applicationset-controller --replicas=1   # your replicas
```

With Helm, `appsetController.replicas: 0` in a `helm upgrade` does the same without leaving a field
conflict for the next upgrade.

Check the output, not only the exit status:

- The first line is `import process started patrol` and the last
  `Import process completed successfully in namespace patrol at …`, for the namespace you meant.
- No `level=WARN` line on stderr. Import also exits 0 when writes are refused (`Forbidden`), when
  the namespace or the CRDs are missing (`NotFound`), and it skips out-of-scope Applications
  silently.
- Every object of the backup exists:

  ```bash
  kubectl -n patrol get -f patrol-backup.yaml -o name >/dev/null && echo complete
  ```

`--dry-run` prints the same lines with `(dry run)` and writes nothing, but it cannot predict
writes the API server will refuse. `--prune` also deletes the objects created since the backup;
see `patrol admin import --help` for what it never deletes.

### After the import

- **Admin password and sessions.** The admin password is the one of the backup; the password the
  new installation generated no longer works, and sessions issued by the new installation before
  the import are refused. Delete the stale initial Secret:
  `kubectl -n patrol delete secret patrol-initial-admin-secret`.
- **Revoked sessions.** The backup restores the signing key but not the list of revoked sessions,
  so a session logged out within the session lifetime (`users.session.duration`, 24 hours by
  default) is valid again until it expires (#289). When that matters, give `patrol-secret` a new
  `server.secretkey` and no `server.secretkey.previous`: every session and account API token is
  then refused, and with Dex the client secret derived from the key must be created again
  ([Single sign-on](installation.md#single-sign-on)).
- **Project-role tokens** are checked against the project's status, which the backup leaves out.
  patrol-server rebuilds it only at startup (#290):
  `kubectl -n patrol rollout restart deployment/patrol-server`.
- **Applications** start without status and are compared again. One whose live state matches Git
  becomes Synced without an Operation. An automated Application that drifted during the outage is
  synced once.
- **SSH known hosts and TLS certificates** reach the repo-server when the kubelet refreshes the
  mounted ConfigMaps, about a minute after the import; SSH repositories can fail until then.
- **Repository credentials.** `patrol repo get URL --refresh hard` runs a connection check with the
  restored credential.

### Restore order by installation type

| Installation | Order |
|--------------|-------|
| Cluster-wide: Helm default, `install.yaml`, `ha/install.yaml` | Install (it creates the namespace and the CRDs, or adopts the kept CRDs); wait for patrol-server and the `default` AppProject; stop the ApplicationSet controller if the backup has ApplicationSets; import; start it again; restart patrol-server if projects have role tokens; delete `patrol-initial-admin-secret` |
| Namespace-scoped: `namespace-install.yaml`, the chart with `rbac.scope: namespaced` | A cluster administrator applies `crds.yaml` of the same version (on a new cluster) and creates the namespace; install; wait as above; import with `--namespace-scoped` (export with it too); the rest as above. The backup holds the Cluster object of the local cluster ([manifests.md](manifests.md#install-a-namespace-scoped-variant)) |
| Core: `core-install.yaml`, the chart's `ci/core.yaml` profile | Install; wait for the controller; import. There is no patrol-server, so no `default` project is created before the import (the backup's comes back with it) and no admin password or session applies |

### A new cluster

When the control-plane cluster is lost, create the new one, install the same version and import as
above. Before the import:

- The destination clusters must be reachable from the new cluster with the credentials in the
  backup. Bearer tokens and client certificates stored in Cluster Secrets keep working as long as
  the destination still has the ServiceAccount or certificate they belong to.
- The Git hosts, Helm and OCI registries must be reachable, and their host keys and CAs must be
  those in the backup's trust ConfigMaps.
- Point the DNS name of `configs.cm.url` at the new installation. SSO keeps working when the URL is
  the same: the redirect URIs do not change, and the Dex client secret is derived from the
  restored key.

Workloads that ran on the lost cluster itself (the in-cluster destination) are gone with it; the
restored Applications show them OutOfSync and Missing until they are synced.

Restoring into another namespace or release name is not tested. The kept CRDs and the admission
policy carry Helm ownership annotations for the old release namespace (install with
`crds.install: false` and `crds.admissionPolicy: false`), and the import copies the old release's
Helm labels and annotations onto `patrol-cm`, `patrol-secret` and the trust ConfigMaps, which can
make the next `helm upgrade` fail.

## Scheduled backups

Patrol CD has no built-in schedule. A CronJob running the installed image can export with the
in-cluster configuration of its ServiceAccount. This one runs in a namespace of its own, so a loss
of the Patrol namespace does not take the backups with it; copy the files out of the cluster as
well (encrypted), since a cluster loss does:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: patrol-backup
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: patrol-backup
  namespace: patrol-backup
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: patrol-backup
  namespace: patrol
rules:
  - apiGroups: [""]
    resources: [configmaps]
    resourceNames: [patrol-cm, patrol-rbac-cm, patrol-ssh-known-hosts-cm, patrol-tls-certs-cm,
      patrol-gpg-keys-cm, patrol-notifications-cm, patrol-cmd-params-cm]
    verbs: [get]
  - apiGroups: [""]
    resources: [secrets]
    verbs: [list]
  - apiGroups: [cd.patrol.example]
    resources: [appprojects, scmproviders, repositories, repositorycredentials, clusters,
      applicationsets, applications]
    verbs: [list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: patrol-backup
  namespace: patrol
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: patrol-backup}
subjects:
  - {kind: ServiceAccount, name: patrol-backup, namespace: patrol-backup}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: patrol-backup
  namespace: patrol-backup
spec:
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: batch/v1
kind: CronJob
metadata:
  name: patrol-backup
  namespace: patrol-backup
spec:
  schedule: "0 2 * * *"
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      backoffLimit: 1
      template:
        spec:
          serviceAccountName: patrol-backup
          restartPolicy: Never
          securityContext:
            runAsNonRoot: true
            runAsUser: 999
            runAsGroup: 999
            fsGroup: 999
            seccompProfile: {type: RuntimeDefault}
          containers:
            - name: export
              image: registry.example.com/patrol-cd/patrol-cd:0.1.0   # the installed version
              command: [/bin/sh, -ec]
              args:
                - |
                  f=/backup/patrol-$(date -u +%Y%m%dT%H%M%SZ).yaml
                  if ! patrol admin export -n patrol -o "$f" 2>/tmp/export.err || grep -q 'level=WARN' /tmp/export.err; then
                    cat /tmp/export.err >&2; rm -f "$f"; exit 1
                  fi
                  find /backup -name 'patrol-*.yaml' -mtime +14 -delete
                  ls -l /backup
              env: [{name: HOME, value: /tmp}]
              securityContext:
                allowPrivilegeEscalation: false
                readOnlyRootFilesystem: true
                capabilities: {drop: [ALL]}
              volumeMounts:
                - {name: backup, mountPath: /backup}
                - {name: tmp, mountPath: /tmp}
          volumes:
            - name: backup
              persistentVolumeClaim: {claimName: patrol-backup}
            - name: tmp
              emptyDir: {}
```

The job fails on any `level=WARN` line, so a refused list does not pass as a backup. With
Applications in other namespaces, also bind a ClusterRole that may list `applications` and
`applicationsets` of `cd.patrol.example` in every namespace. Take the chart version and values
along with the files when you copy them out. Check that the job succeeds (alert on failed Jobs
of the CronJob), and restore a backup now and then into a scratch cluster: a backup that was
never restored is not known to work.

## Exporting or importing in the wrong namespace

Upstream's disaster recovery page warns that export does not fail in the wrong namespace. Patrol's
export does fail in a namespace without an installation, with `error getting ConfigMap
<namespace>/patrol-cm` and no file written (`TestExportRequiresSettingsConfigMaps`). It succeeds
without complaint in a namespace that holds another installation.

Import has no such check. In a namespace without Patrol CD it writes the whole backup there,
credentials included, and reports success; in a namespace that does not exist it writes nothing,
prints warnings and still exits 0. Pass `-n`, read the `import process started <namespace>` line,
and use `--dry-run` first when in doubt (#287). If credentials were written to the wrong namespace, delete
them there, and rotate them if others can read that namespace.
