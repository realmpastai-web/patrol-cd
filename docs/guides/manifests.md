# Installing Patrol CD from the static manifests

`deploy/manifests` holds ready-to-apply install manifests for clusters where Helm is not used:
single files for `kubectl apply` and kustomize bases with the same objects for overlays. They
are generated from the Helm chart (`deploy/helm/patrol-cd`, see [installation.md](installation.md))
by `hack/genmanifests`, so a static install and a Helm install with the same values run the same
objects. Upstream counterpart: Argo CD's `manifests/*.yaml` (OPS-001 to OPS-006).

Status: the files are generated, checked against the chart renders and applied to a local
kube-apiserver 1.36 (server-side apply, strict field validation, Pod Security `restricted`
warnings, idempotent re-apply), recorded in
[`docs/verification/runs/2026-10-17-manifests-static.md`](../verification/runs/2026-10-17-manifests-static.md).
On a cluster with nodes they ran once, by hand: on 2026-10-19 `install.yaml` (the
`cluster-install` base with a local image overlay) was applied to a local kind cluster (61
objects; five pods Running without restarts, the bootstrap converged, admin login through a
port-forward) and removed again
([run record](../verification/runs/2026-10-19-kind-validation.md), step 14). No committed check
applies a bundle to a cluster with nodes, and the other variants, NetworkPolicy enforcement of
the static bundles and HA scheduling are unverified. No Patrol image is published (`make image`
builds `patrol-cd/patrol-cd:dev` locally).

## Contents

| Single file | Kustomize base | What it installs |
|-------------|----------------|------------------|
| `install.yaml` | `cluster-install/` (also `deploy/manifests` itself) | Cluster-wide: namespace `patrol`, CRDs, Operation admission policy, patrol-server (API, UI, webhooks), controller with cluster-admin-equivalent rights on the local cluster, repo-server, ApplicationSet and notifications controllers |
| `install-with-hydrator.yaml` | `cluster-install-with-hydrator/` | `install.yaml` + source hydrator (commit-server, `hydrator.enabled`) |
| `namespace-install.yaml` | `namespace-install/` | Same components with Roles only: no namespace, CRDs, admission policy or ClusterRoles; on the local cluster only the `patrol` namespace can be managed, once the local cluster is registered with that namespace ([Install a namespace-scoped variant](#install-a-namespace-scoped-variant)) |
| `namespace-install-with-hydrator.yaml` | `namespace-install-with-hydrator/` | `namespace-install.yaml` + source hydrator |
| `core-install.yaml` | `core-install/` | Headless: namespace, CRDs, admission policy, controller (ClusterRole), repo-server, ApplicationSet controller; no patrol-server, UI, Dex or notifications controller |
| `core-install-with-hydrator.yaml` | `core-install-with-hydrator/` | `core-install.yaml` + source hydrator |
| `ha/install.yaml` | `ha/cluster-install/` (also `ha/`) | `install.yaml` with replicated components (3 or more nodes) |
| `ha/install-with-hydrator.yaml` | `ha/cluster-install-with-hydrator/` | HA + source hydrator |
| `ha/namespace-install.yaml` | `ha/namespace-install/` | `namespace-install.yaml` with replicated components |
| `ha/namespace-install-with-hydrator.yaml` | `ha/namespace-install-with-hydrator/` | HA namespaced + source hydrator |
| `crds.yaml` | `crds/` | The 8 CRDs and the Operation ValidatingAdmissionPolicy with its binding (for namespace-scoped installations, installed once per cluster) |
| — | `namespace/` | The namespace `patrol` with Pod Security admission labels |

Every single file equals `kustomize build` of its base (plus a header comment); `make
verify-manifests` checks both against the chart. `profiles/*.yaml` are the chart value files of
the variants; do not edit the generated files.

All objects carry `namespace: patrol` (cluster-scoped ones none) and the chart's labels for
release `patrol` (`app.kubernetes.io/instance: patrol`, `app.kubernetes.io/part-of: patrol-cd`,
`app.kubernetes.io/name`, `app.kubernetes.io/component`, `app.kubernetes.io/version`). Applying a
file to another namespace with `-n` fails loudly (`the namespace from the provided object
"patrol" does not match ...`); use a kustomize overlay to change it (see
[Another namespace](#another-namespace)).

## Choosing a variant

| Need | Variant |
|------|---------|
| Deploy to the cluster Patrol runs in (and others) | `install.yaml` |
| No cluster-wide rights for the Patrol installation (several teams' instances, external clusters only) | `namespace-install.yaml` (+ `crds.yaml` by a cluster administrator) |
| GitOps engine only, managed with kubectl or `patrol --core` | `core-install.yaml` |
| Production with node failures tolerated | `ha/install.yaml` or `ha/namespace-install.yaml` |
| Source hydrator (rendered manifests pushed to Git) | any `*-with-hydrator.yaml` |
| SSO through the bundled Dex, internal TLS from cert-manager, Ingress/Gateway objects, ServiceMonitors, CMP sidecars, custom RBAC rules, air-gapped mirrors | the Helm chart (or your own overlay); the static variants use the chart defaults |

The variants differ from each other only in the chart values in `deploy/manifests/profiles`:

| Profile | Values |
|---------|--------|
| `install.yaml` | chart defaults |
| `namespace-install.yaml` | `crds.install: false`, `crds.admissionPolicy: false`, `rbac.scope: namespaced` |
| `core-install.yaml` | `server.enabled: false`, `dex.enabled: false`, `notificationsController.enabled: false` |
| `ha.yaml` | 2 replicas of patrol-server and repo-server with required anti-affinity per node; 2 controller shards; 2 ApplicationSet and notifications controllers and commit-servers; preferred anti-affinity per node and zone spread for the rest |
| `hydrator.yaml` | `hydrator.enabled: true` |

## Prerequisites

| Requirement | Notes |
|-------------|-------|
| Kubernetes `>= 1.30` | ValidatingAdmissionPolicy is GA from 1.30. The Argo CD v3.5.5 baseline is tested on 1.33–1.36. |
| kubectl with server-side apply | The ApplicationSet CRD (1.4 MB of YAML) exceeds the 256 KiB `last-applied-configuration` annotation of client-side apply: `The CustomResourceDefinition "applicationsets.cd.patrol.example" is invalid: metadata.annotations: Too long`. Always pass `--server-side`. |
| The Patrol image in a registry your nodes can pull | The manifests reference `patrol-cd/patrol-cd:0.1.0`, which is not published. Build and push it ([images.md](images.md)) and point the manifests at it with a kustomize overlay ([The image](#the-image)). |
| Installer permissions | Cluster-wide and core variants create CRDs, a ValidatingAdmissionPolicy, ClusterRoles and the namespace: cluster-admin. Namespace-scoped variants (after a cluster administrator installed `crds.yaml`) create Roles only, but a namespace `admin` cannot apply them: RBAC escalation prevention refuses 6 of the 7 Roles (`Error from server (Forbidden): … is attempting to grant RBAC permissions not currently held`), because they grant the Patrol CRD verbs and namespace-wide `*` rights the `admin` role does not hold, and their RoleBindings then fail too. Apply them as cluster-admin, as a user bound to the `cluster-admin` ClusterRole by a RoleBinding in the namespace, or as a namespace `admin` who also holds `escalate` and `bind` on Roles in the namespace ([Install a namespace-scoped variant](#install-a-namespace-scoped-variant)). |
| Nodes | HA variants: at least three schedulable nodes (two replicas with required anti-affinity plus a surge pod during rolling updates), preferably in two or more zones. |
| A CNI that enforces NetworkPolicies | Optional but assumed: every component restricts ingress and egress ([installation.md](installation.md#networkpolicies)). |

## The image

Point every Patrol container (init containers included) at your registry with a kustomize
overlay; pin a digest for production:

```yaml
# kustomization.yaml of your overlay
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
sortOptions:
  order: fifo                                       # keep the base's apply order (see below)
resources:
  - ../patrol-cd/deploy/manifests/cluster-install   # a checkout or vendored copy of this repository
images:
  - name: patrol-cd/patrol-cd
    newName: registry.example.com/patrol-cd/patrol-cd
    newTag: 0.1.0
    # digest: sha256:<digest>                       # replaces the tag
```

```bash
kubectl kustomize ./overlay | kubectl apply --server-side --force-conflicts -f -
# or: kubectl apply --server-side --force-conflicts -k ./overlay
```

Kustomize can also read a base from Git (`https://github.com/juliosaraiva/patrol-cd//deploy/manifests/cluster-install?ref=<tag>`);
pin `ref` to a release tag.

## Install a cluster-wide variant

```bash
kubectl apply --server-side --force-conflicts -f deploy/manifests/install.yaml
kubectl -n patrol rollout status statefulset/patrol-controller
kubectl -n patrol rollout status deployment/patrol-server deployment/patrol-repo-server \
  deployment/patrol-applicationset-controller deployment/patrol-notifications-controller
```

(With an image overlay, apply the overlay instead.) Substitute `ha/install.yaml`,
`core-install.yaml` or a `*-with-hydrator.yaml` file as needed; the rollout list follows the
components the variant contains.

- The file starts with the namespace, then the CRDs and the admission policy, NetworkPolicies and
  PDBs, ServiceAccounts, Secrets and ConfigMaps, RBAC, Services and finally the workloads, so
  everything a pod needs exists before it starts. An overlay keeps that order only with
  `sortOptions: {order: fifo}` ([Customizing with kustomize](#customizing-with-kustomize)).
- The namespace is created with Pod Security admission `enforce: restricted` (pinned to the
  v1.30 policy) and `audit`/`warn: restricted` (latest policy); every Patrol pod complies. Adding
  a sidecar or a workload to this namespace that needs more privileges requires changing these
  labels (patch the `Namespace` in an overlay).
- `--force-conflicts` makes kubectl take ownership of fields another manager set (for example a
  field changed with `kubectl edit`); without it a re-apply stops at the first conflict.
- What happens at first start (runtime Secrets, init container, mount retries) is described in
  [installation.md](installation.md#install).

## Install a namespace-scoped variant

A cluster administrator installs the CRDs and the admission policy once per cluster (they are
shared by every installation in the cluster) and creates the namespace:

```bash
kubectl apply --server-side --force-conflicts -f deploy/manifests/crds.yaml
kubectl apply --server-side -f deploy/manifests/namespace/namespace.yaml   # or: kubectl create namespace patrol
```

Patrol is then installed by someone who may grant the Roles' permissions in that namespace.
A namespace `admin` may not (see [Prerequisites](#prerequisites)); the cluster administrator
either applies the file, or delegates the namespace with one of these bindings (`team-lead` is
an example user):

```bash
# full rights inside the namespace only
kubectl -n patrol create rolebinding patrol-installer --clusterrole=cluster-admin --user=team-lead
# or: keep the admin role and allow granting Roles beyond it
kubectl -n patrol create rolebinding patrol-installer-admin --clusterrole=admin --user=team-lead
kubectl -n patrol create role patrol-rbac-escalate --verb=escalate,bind --resource=roles.rbac.authorization.k8s.io
kubectl -n patrol create rolebinding patrol-rbac-escalate --role=patrol-rbac-escalate --user=team-lead
```

```bash
kubectl apply --server-side --force-conflicts -n patrol -f deploy/manifests/namespace-install.yaml
```

`TestNamespaceInstallPermissions` (`test/deploy`, kube-apiserver 1.36) applies the file as each
persona: the plain namespace `admin` is refused 6 Roles, both delegations succeed. (envtest runs
no kube-controller-manager, so the test builds the `admin` role's rules from the ClusterRoles it
aggregates, `system:aggregate-to-admin`, `-edit` and `-view`.)

Semantics (as upstream `namespace-install.yaml`): no ClusterRole is created; the controller and
patrol-server hold Roles in the `patrol` namespace only. **Register the local cluster before an
Application targets it**, even with destination namespace `patrol`. Without a Cluster object for
`https://kubernetes.default.svc`, the controller uses a built-in in-cluster cluster without a
namespace list: its live-state cache then lists every resource type cluster-wide, the Roles
refuse that, and the Applications' comparisons fail. A Cluster object listing the namespaces
limits the cache to them (no credentials: the controller uses its own ServiceAccount):

```bash
kubectl apply -n patrol -f - <<'EOF'
apiVersion: cd.patrol.example/v1alpha1
kind: Cluster
metadata:
  name: in-cluster
spec:
  name: in-cluster
  server: https://kubernetes.default.svc
  namespaces: [patrol]
EOF
```

Applications may then deploy namespaced resources into `patrol` on the local cluster. Checked
with the chart's equivalent profile: `TestRBACSufficiency/namespaced` (`test/deploy`) renders
`ci/namespaced.yaml` (the same Roles plus `rbac.managedNamespaces: [team-a]`), creates such a
Cluster object (namespaces: the release namespace and `team-a`) and syncs Applications into the
release namespace with the real components. The static file itself has not been run with the
components.

- More namespaces on the local cluster: add them to `spec.namespaces` and bind Roles there for
  the `patrol-controller` ServiceAccount (and `patrol-server` for live views): the rules of the
  `patrol-controller-local` and `patrol-server-local` Roles (the chart's
  `rbac.managedNamespaces` creates them for Helm installs).
- With a namespace overlay ([Another namespace](#another-namespace)), change
  `metadata.namespace` and `spec.namespaces` of this Cluster object as well: it is not part of
  the base.
- Other clusters are reached with the credentials of their Cluster objects.

## Core (headless) installation

```bash
kubectl apply --server-side --force-conflicts -f deploy/manifests/core-install.yaml
patrol app list --core -n patrol             # the CLI talks to Kubernetes directly
```

A core installation has no patrol-server, which is the only component that creates the AppProject
`default`: create it (or your own project) before the first Application
(DV-2 in `docs/verification/phase4-findings.md`):

```bash
kubectl apply -n patrol -f - <<'EOF'
apiVersion: cd.patrol.example/v1alpha1
kind: AppProject
metadata:
  name: default
spec:
  sourceRepos: ["*"]
  destinations:
    - {server: "*", namespace: "*"}
  clusterResourceWhitelist:
    - {group: "*", kind: "*"}
EOF
```

That is the permissive spec patrol-server creates; use a narrower project for real tenants.

The bootstrap init container runs on the controller. Like upstream `core-install.yaml`, the
notifications controller is not included (the Helm `ci/core.yaml` profile keeps it); install
`install.yaml` or use the chart with `server.enabled: false` if you need notifications.

## First login

```bash
patrol admin initial-password -n patrol          # prints the generated admin password
kubectl -n patrol port-forward service/patrol-server 8080:443
patrol login localhost:8080 --username admin --insecure   # self-signed certificate
patrol account update-password
kubectl -n patrol delete secret patrol-initial-admin-secret
```

The UI is at <https://localhost:8080> while the port-forward runs. Set the external URL
(`url` in `patrol-cm`) before you use SSO, notifications or links, and expose patrol-server with
your own Ingress, Gateway route or LoadBalancer Service (the static manifests contain none; the
Service is `patrol-server`, ports 80 and 443 to container port 8080, HTTPS unless
`server.insecure`). TLS options are described in
[installation.md](installation.md#exposing-the-api-and-ui).

## Configuration

The manifests ship the settings objects with the chart defaults: `patrol-cm` (the default
resource exclusions and `ignoreResourceUpdates` rules), `patrol-cmd-params-cm` (the wiring
keys: `repo.server`, and with the hydrator `hydrator.enabled` and `commit.server`),
`patrol-rbac-cm`, the trust ConfigMaps (`patrol-ssh-known-hosts-cm`, `patrol-tls-certs-cm`,
`patrol-gpg-keys-cm`, all without data), `patrol-notifications-cm`, and two empty Secrets
(`patrol-secret`, `patrol-notifications-secret`) filled at runtime or by you. The bootstrap init
container writes the published host keys of GitHub, Bitbucket Cloud, GitLab.com and Azure DevOps
into `patrol-ssh-known-hosts-cm` once (key `ssh_known_hosts`, only while it is absent); from then
on known hosts, TLS certificates and GnuPG keys are managed through the API and CLI
(`patrol cert …`, `patrol gpg …`, the UI), and re-applying the manifests keeps those changes
(`TestTrustConfigMapsStaticReapply` in `test/deploy`).

Re-applying the manifests resets the fields they contain. Keep your configuration where a
re-apply keeps it:

- Preferred: an overlay that patches the settings objects, so every apply carries your
  configuration (strategic merge patches merge `data` keys):

  ```yaml
  patches:
    - patch: |-
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: patrol-cm
          namespace: patrol
        data:
          url: https://patrol.example.com
    - patch: |-
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: patrol-cmd-params-cm
          namespace: patrol
        data:
          reposerver.parallelism.limit: "10"
  ```

- Keys you add with `kubectl edit` or `kubectl patch` survive a re-apply; changes to keys the
  manifests ship (for example `resource.exclusions`) are reverted by `--force-conflicts`.
- Trust material: patching `ssh_known_hosts` (or TLS/GnuPG entries) into the trust ConfigMaps in
  your overlay makes kubectl own those keys, so every re-apply with `--force-conflicts` reverts
  API changes to them (without `--force-conflicts` the re-apply stops at the conflict). Manage a
  key either in the overlay or through the API, not both.
- If you manage Secrets such as `patrol-notifications-secret` with `kubectl apply --server-side`
  from your own files, use your own field manager (`--field-manager=patrol-config`). Both
  applies would otherwise use kubectl's default manager, and re-applying the empty Secret of the
  install manifest would remove the keys your earlier apply set.
- Never put credentials into an overlay committed to Git; create the Secrets out of band or with
  a secrets operator.

`patrol-cmd-params-cm` is read at startup. The pod templates carry the chart's
`checksum/cmd-params` annotation of the shipped ConfigMap, so a new release that changes it rolls
the pods; after changing the ConfigMap yourself, restart the components
(`kubectl -n patrol rollout restart deployment,statefulset -l app.kubernetes.io/part-of=patrol-cd`)
or patch the annotation in your overlay. The other settings objects are followed live.

## Customizing with kustomize

Use the directories as bases, never edit the generated files. Supported transformations:
`images`, `patches` (settings objects, resources, replicas, scheduling, NetworkPolicy peers),
`namespace`, `labels` and `commonAnnotations`. Notes:

- Add labels with `labels: [{pairs: {...}, includeSelectors: false}]`. `commonLabels` (or
  `includeSelectors: true`) also changes selectors: Deployment and StatefulSet selectors are
  immutable, so the next apply fails, and NetworkPolicy peers stop matching.
- `namePrefix`/`nameSuffix` are not supported: the binaries read the settings objects by fixed
  names (`patrol-cm`, `patrol-secret`, ...), and component addresses (`patrol-repo-server:8081`)
  are wired in `patrol-cmd-params-cm` and arguments.
- Changing replicas of patrol-server, the repo-server and the ApplicationSet/notifications
  controllers is a plain patch. Changing the controller's replicas also needs the
  `PATROL_CONTROLLER_REPLICAS` environment variable of the `controller` container to match (the
  number of shards). `kubectl scale` works until the next re-apply, which resets the replicas
  the file ships (`--force-conflicts`); keep the count in your overlay.
- Apply order: the bases list their objects in the generator's order (namespace, CRDs and
  admission policy, NetworkPolicies and PDBs, ServiceAccounts, Secrets and ConfigMaps, RBAC,
  Services and finally the workloads) with `sortOptions: fifo`, but an overlay applies its own
  `sortOptions`.
  Without one, kustomize's legacy order emits the Deployments and the StatefulSet before the
  PodDisruptionBudgets, the admission policy and the NetworkPolicies, so pods can start before
  their NetworkPolicies exist. Put `sortOptions: {order: fifo}` in every overlay, as in the
  examples here: `kustomize build` (v5.8.1) of the image overlay above then lists the 61
  objects of `cluster-install` in exactly the order of `install.yaml`.
- Applying a directory with `sortOptions` (a base, or an overlay as above) with
  `kubectl apply -k` prints `Warning: Sorting order is set both in 'kustomization.yaml'
  ('sortOptions') and in a CLI flag`. It is harmless: the kustomization's order is used.

### Another namespace

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: patrol-prod
sortOptions:
  order: fifo
resources:
  - ../patrol-cd/deploy/manifests/cluster-install
```

Kustomize renames the `Namespace`, moves every namespaced object and updates the ServiceAccount
subjects of the RoleBindings and ClusterRoleBindings. Two side effects, checked against a
kube-apiserver 1.36: the cluster-scoped objects keep their names (`patrol-patrol-controller`,
...; one cluster-wide installation per cluster anyway), and kustomize, which does not know that
ValidatingAdmissionPolicy and its binding are cluster-scoped, adds `metadata.namespace` to
them; the API server ignores it for cluster-scoped kinds (the stored objects have no namespace).
The release name (`app.kubernetes.io/instance: patrol`) cannot be changed in static manifests;
use Helm for that.

## Upgrade

Take a backup first ([backup-restore.md](backup-restore.md)). Apply the new version's file (or
overlay) the same way as the installation:

```bash
kubectl apply --server-side --force-conflicts -f deploy/manifests/install.yaml
```

- The CRDs and the admission policy are part of the cluster-wide files and are upgraded with
  them. For namespace-scoped installations, the cluster administrator applies the new
  `crds.yaml` first (once per cluster; all installations of a cluster must be compatible with it).
- Fields removed from an object between versions are removed from the live object (kubectl's
  field manager owned them). Objects removed between versions are not deleted: `kubectl apply`
  does not prune. The release notes list removed objects; delete them explicitly.
- Switching variants works the same way, with the same caveat: going from a `*-with-hydrator`
  file to its plain counterpart removes `hydrator.enabled` and `commit.server` from
  `patrol-cmd-params-cm` but leaves the commit-server objects; delete them:

  ```bash
  kubectl -n patrol delete deployment,service,serviceaccount,networkpolicy,poddisruptionbudget \
    -l app.kubernetes.io/instance=patrol,app.kubernetes.io/component=commit-server
  ```

  Likewise, `install.yaml` to `core-install.yaml` leaves the objects of patrol-server and the
  notifications controller (Deployments, Services, ServiceAccounts, Roles, the server's
  ClusterRole and their bindings, NetworkPolicies, PDBs, `patrol-notifications-cm` and
  `-secret`); compare `kubectl get -n patrol all,cm,secret,sa,role,networkpolicy,pdb -l
  app.kubernetes.io/instance=patrol` with the new file before deleting them.
- Runtime material (`server.secretkey`, the admin password, the internal token in
  `patrol-internal-token`) is not part of the manifests and survives upgrades, as do the known
  hosts, TLS certificates and GnuPG keys managed through the API ([Configuration](#configuration)).
- Rollback: apply the previous version's file. CRD schemas are additive within `v1alpha1`;
  check the release notes before rolling back across versions.

## Uninstall

`kubectl delete -f install.yaml` deletes everything the file contains, including the CRDs (and
with them every Application, AppProject, Cluster, Repository, ... in the cluster) and the
namespace (and with it the runtime Secrets). Do it in this order:

1. Delete the Applications while the controller still runs, so their finalizers can clean up
   (cascading deletion removes the deployed resources; remove the finalizer first for
   Applications whose resources must stay):

   ```bash
   kubectl delete applications.cd.patrol.example --all --all-namespaces --wait
   ```

2. Remove the installation:

   ```bash
   kubectl delete -f deploy/manifests/install.yaml --ignore-not-found      # everything
   # or keep the CRDs, the admission policy and the namespace (and the objects in them):
   kubectl delete -f deploy/manifests/cluster-install/resources.yaml --ignore-not-found
   ```

   Namespace-scoped: `kubectl delete -n patrol -f deploy/manifests/namespace-install.yaml`, then
   the cluster administrator removes `crds.yaml` when no installation uses it.

3. With the namespace kept, delete the Secrets created at runtime:
   `kubectl -n patrol delete secret patrol-internal-token patrol-initial-admin-secret`.

If the controller is gone before its Applications, their finalizers block the deletion of the
Applications, the namespace and the CRDs. Remove them explicitly:
`kubectl patch applications.cd.patrol.example <name> -n <namespace> --type merge -p
'{"metadata":{"finalizers":null}}'`.

## Differences between Helm and static installations

| Topic | Helm chart | Static manifests |
|-------|------------|------------------|
| Objects | Rendered from your values | The chart rendered with `profiles/*.yaml`; identical objects (checked by `make verify-manifests`) except the two rows below |
| Namespace | Not rendered (`--create-namespace` creates it without labels) | Cluster-wide variants include it, with Pod Security `restricted` labels; namespace-scoped variants do not |
| Helm metadata | `helm.sh/chart`, `app.kubernetes.io/managed-by: Helm`, CRDs with `helm.sh/resource-policy: keep` | None: no `managed-by` label (applied by kubectl, kustomize, a GitOps tool or Patrol itself, so no single value would be true; upstream's static manifests carry none either), no keep annotation |
| Release name and namespace | Any (one release per namespace) | Release `patrol`; namespace `patrol`, changeable with a kustomize overlay |
| Configuration | Values, validated by `values.schema.json` and the settings register (a typo fails the install) | Overlay patches of the settings objects; not validated before apply (invalid settings show up in the component logs) |
| Optional integrations | Dex, cert-manager internal TLS and certificates, Ingress, HTTPRoute, ServiceMonitor, PrometheusRule, CMP sidecars, extra RBAC | Not included (chart defaults); add them in an overlay or use Helm |
| Config changes roll pods | Yes (`checksum/cmd-params` follows your values) | Only when a release changes the shipped ConfigMap; restart or patch the annotation after your own changes |
| Upgrade | `helm upgrade`: three-way merge, deletes objects removed from the chart | `kubectl apply --server-side`: field ownership, no pruning of removed objects |
| Rollback | `helm rollback` | Apply the previous version's file |
| Uninstall | Keeps the CRDs and the admission policy (`crds.keep`) | Deletes whatever the file lists, CRDs and namespace included (see [Uninstall](#uninstall)) |
| Adoption | — | To move a static installation under Helm, label and annotate every object (`app.kubernetes.io/managed-by=Helm`, `meta.helm.sh/release-name=patrol`, `meta.helm.sh/release-namespace=patrol`) before `helm install`; Helm refuses objects without them |

## Differences from upstream Argo CD's manifests

| Upstream (v3.5.5) | Patrol | Reason |
|-------------------|--------|--------|
| Objects without `metadata.namespace`, applied with `-n argocd`; ClusterRoleBindings name namespace `argocd` (another namespace needs a patch, OPS-001) | `namespace: patrol` on every namespaced object; another namespace through a kustomize `namespace:` overlay, which also updates the bindings | a wrong `-n` fails instead of leaving the controller without cluster rights |
| No Namespace object | Namespace with Pod Security labels in the cluster-wide variants | one-step install; the restricted profile is enforced |
| Redis (`argocd-redis`, Redis HA in `ha/`) | none | ADR-0004 |
| 3 CRDs | 8 CRDs and the Operation ValidatingAdmissionPolicy (with binding) | resource model (ADR-0003); the admission policy guards Operation authorship |
| `ha/`: controller 1 replica, ApplicationSet and notifications controllers 1 replica | controller 2 replicas (2 Lease-coordinated shards with takeover, ADR-0008), ApplicationSet and notifications controllers 2 (leader election), commit-server 2 | failover without waiting for rescheduling |
| `ha/*-with-hydrator.yaml` deploy the commit-server without setting `hydrator.enabled` | every `*-with-hydrator` file sets it | upstream quirk not reproduced (OPS-005) |
| NetworkPolicies restrict ingress only | ingress and egress restricted per component | [installation.md](installation.md#networkpolicies) |
| Dex in `install.yaml` (`argocd-dex-server`) | not in the static manifests (Helm `dex.enabled`) | Patrol has no Dex configuration launcher (OPS-152); Dex needs a client secret created after install |

## Regenerating (contributors)

```bash
make manifests-install    # render the chart, write deploy/manifests (needs helm 4 and bin/kustomize)
make verify-manifests     # fail when deploy/manifests is stale; writes nothing
make generate             # CRDs -> chart (helm-docs) -> deploy/manifests
make verify-generated     # includes deploy/manifests
```

`hack/genmanifests` runs `helm template patrol deploy/helm/patrol-cd --namespace patrol
--kube-version 1.36.0` with each variant's value files in an empty Helm home (no repositories,
plugins or cache, no cluster access), removes `helm.sh/chart`, `app.kubernetes.io/managed-by:
Helm` and `helm.sh/*` annotations, refuses Helm hooks and tests, Helm metadata in selectors and
Secrets with data, checks each variant's scope (namespace-scoped variants contain no
cluster-scoped object; every namespaced object is in `patrol`), sorts the objects into apply
order, writes the kustomize bases (formatted by kustomize v5.8.1, the version pinned in
`tools/go.mod`) and builds the single files from them. Before writing, it compares every single
file, parsed the way kubectl parses YAML, with the chart render cleaned by separate code: any
missing, extra or different object fails the run. The output is byte-for-byte reproducible:
there are no timestamps, random values or tool versions in it, and two runs produce identical
files (`hack/genmanifests:TestManifestsAreCurrent`).

To change what a variant contains, change the chart or `deploy/manifests/profiles/*.yaml` and
regenerate. To add a variant, add it to the `variants` table in `hack/genmanifests/main.go`.

## Known limitations

- Applied to a cluster with nodes only once, by hand (`install.yaml` on kind, see Status); the
  other variants, NetworkPolicy enforcement and HA scheduling are unverified.
- The image is not published; an image overlay is required.
- No Dex, Ingress or monitoring objects in the static variants (use Helm or an overlay).
- `kubectl apply` does not prune objects removed between versions or variants.
