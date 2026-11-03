# Local development environment (kind)

Two disposable kind clusters, a local OCI registry and in-cluster e2e fixtures (git server,
Helm chart repository, OCI artifacts) for developing and testing Patrol CD on a workstation.

```
 host (macOS/Linux)                       docker network "kind"
 ─────────────────                        ───────────────────────────────────────────────────────────
 127.0.0.1:8443 ──────────────▶ patrol-control-control-plane :30443 (NodePort, Patrol UI/API)
 127.0.0.1:<random> ──────────▶ patrol-control-control-plane :6443   ┐ patrol-control
                                patrol-control-worker               ┘ (fixtures in ns patrol-e2e)
 127.0.0.1:<random> ──────────▶ patrol-target-control-plane  :6443   ◀── pods in patrol-control
                                                                          https://patrol-target-control-plane:6443
 127.0.0.1:5001 ──────────────▶ patrol-registry              :5000   ◀── pods: patrol-registry:5000
                                                                     ◀── node containerd: localhost:5001
```

Everything here is disposable: the scripts only create, change or delete kind clusters named
`patrol-*`, the `patrol-registry` container and local images tagged `patrol-cd/*:dev`.

## Requirements

| Tool | Tested version |
|------|----------------|
| Docker (Docker Desktop on macOS) | 29.8 |
| kind | v0.33.0 |
| kubectl | v1.36 |
| helm | v4.3 (`oci/push.sh`, `chart-repo/build.sh`) |
| git, openssl, ssh-keygen, curl | any recent |

Resources: three kind nodes (Kubernetes 1.36.4, `kindest/node:v1.36.4@sha256:099e…`, pinned in
`hack/kind/lib.sh`), the registry and the fixtures. As an estimate (not yet measured), plan for
about 4 GiB of free memory in the Docker VM and at least 5 GB of free disk in it (node image,
three node containers, fixture images). Check the disk with `docker system df`: when Docker's disk is full every container
start fails with `no space left on device` (see [Troubleshooting](#troubleshooting)).

Network: needed to pull the node, registry, `alpine` and `oras` base images and, on the first
fixture image build, for the `apk add` step of each fixture Dockerfile. After one successful
build, `test/e2e/fixtures/deploy.sh --skip-build` works offline, and rebuilds do too while the
build cache keeps the `apk add` layers. The exact image references and `docker pull` commands
are in [test/e2e/fixtures/README.md](../../test/e2e/fixtures/README.md#network-and-offline-use).

The scripts are written for bash 3.2 (macOS `/bin/bash`) and POSIX `sh` (in-container scripts).

## Create the clusters

```bash
make kind-up        # = hack/kind/up.sh
```

`up.sh` is idempotent. It:

1. creates (or starts) the registry container `patrol-registry` (`registry:3.0.0`) publishing
   `127.0.0.1:5001`, labelled `cd.patrol.example.kind-role=registry`;
2. creates `patrol-control` (control plane + worker, `hack/kind/patrol-control.yaml`) and
   `patrol-target` (one node, `hack/kind/patrol-target.yaml`), or starts their stopped node
   containers;
3. attaches the registry to the `kind` network and writes containerd `hosts.toml` files on every
   node;
4. waits for nodes `Ready` and CoreDNS, and publishes the KEP-1755 `local-registry-hosting`
   ConfigMap in `kube-public`;
5. writes kubeconfigs to `.cache/kind/` and checks that the `patrol-control` control-plane
   container reaches `https://patrol-target-control-plane:6443/version` with TLS verified.

Override the node or registry image with `PATROL_KIND_NODE_IMAGE` / `PATROL_REGISTRY_IMAGE`.

Other targets in `hack/kind/kind.mk`: `kind-down`, `kind-load`, `kind-status`, `kind-fixtures`,
`kind-install`, `kind-smoke`, `kind-verify` and `kind-restore-drill`.

## Install Patrol CD and verify it

The full sequence from nothing:

```bash
make image                    # builds patrol-cd/patrol-cd:dev (kind-install needs it)
make kind-up kind-fixtures    # clusters, registry, git server, chart repository, OCI artifacts
make kind-install             # load the image, helm install, register patrol-target and the fixtures
make kind-verify              # 42 checks against the live clusters
make kind-verify VERIFY_FLAGS=--lifecycle   # 58 checks: also rollback, password change, uninstall, re-install
make kind-restore-drill       # 59 checks: export, delete the namespace, install again, import
RESTORE_DRILL_FLAGS=--new-cluster make kind-restore-drill   # the same, deleting and recreating patrol-control
```

`hack/kind/restore-drill.sh` runs the procedure of [backup-restore.md](backup-restore.md) and
checks the restored installation (see the guide's status paragraph). It needs the state
`kind-install kind-verify` leaves, and leaves the clusters in that state when it passes; when it
fails after the loss, it prints the targets that rebuild the installation.

`hack/kind/install.sh` loads the image into `patrol-control`, runs
`helm upgrade --install patrol deploy/helm/patrol-cd` with `hack/kind/values.yaml` (the local
image, the UI and API on NodePort 30443 = `https://127.0.0.1:8443`, and the extra repo-server
egress ports below), waits for the workloads, then runs `hack/kind/register-target.sh` and
`test/e2e/fixtures/patrol/register.sh --with-applications`. The initial admin password is in
Secret `patrol-initial-admin-secret`. The Applications are synced manually.

The image tag is always `dev`, so after a `make image` the pod templates do not change and helm
alone would keep the old pods: when the release already exists and an image was loaded,
`install.sh` restarts the Patrol CD workloads. It also waits until the repo-server sees the
fixture git server's host key in its mounted known-hosts file (the kubelet refreshes a mounted
ConfigMap with a delay of about a minute; until then the sync of `e2e-guestbook-ssh` is refused with
412 `No ED25519 host key is known … strict checking`, KV-12).

`hack/kind/verify-install.sh` is the check list: pods ready without restarts; the user, the
capability bounding set, `no_new_privs` and the read-only root file system of every component; the bootstrap; the API
(health, anonymous refusal, login, default project); the repo-server's NetworkPolicy; metrics;
a sync of every fixture Application through the API with the workloads checked on
`patrol-target`; and drift detection. With `--lifecycle` it also rolls the release back to
revision 1 and upgrades it again, changes the admin password through the API, uninstalls and
re-installs (the CRDs, the Applications and the workloads must survive, the changed and the old
initial password must be refused and a new initial password must work), and finally registers
the fixtures again, because the re-install drops the fixture git server's host key, and waits
until e2e-guestbook-ssh has been compared again. The sync, drift and lifecycle sections change
state on the clusters; each runs only when every earlier check passed, and a failed lifecycle
step stops the run with the commands that restore the installation. It prints PASS or FAIL per
check and exits 1 on any failure.

**NetworkPolicy ports are pod ports.** The chart's policies match the destination pod's port after
Service translation. The fixtures listen on 8080 (git over HTTP, chart repository) and 2222 (git
over SSH) behind Services on 80 and 22, and `patrol-registry` listens on 5000, so
`hack/kind/values.yaml` sets `networkPolicy.repoServerEgress.ports` to
`[22, 80, 443, 2222, 5000, 8080]`. With the chart's default ports (22, 80, 443) every git and OCI
request of the repo-server hangs until its timeout. The same applies to any in-cluster Git server,
registry or chart repository you point Patrol CD at.

The `patrol` CLI in core mode (`patrol --core --kube-context kind-patrol-control app list`)
works against `patrol-control`. Its commands that read a destination cluster's resources (logs,
live resources) need a kubeconfig context whose server URL equals the Cluster's `spec.server`;
the kind Cluster uses the container-network name `https://patrol-target-control-plane:6443`, which
the host cannot reach, so those commands answer `core mode: no kubeconfig context for destination
cluster ...` from the host.

## Kubeconfigs and the context guard

kind is always called with `--kubeconfig`, so `~/.kube/config` is never read or modified.

| File (`.cache/kind/`) | Context | Server |
|-----------------------|---------|--------|
| `patrol-control.kubeconfig` | `kind-patrol-control` | `https://127.0.0.1:<port>` |
| `patrol-target.kubeconfig` | `kind-patrol-target` | `https://127.0.0.1:<port>` |
| `patrol-target-internal.kubeconfig` | `kind-patrol-target` | `https://patrol-target-control-plane:6443` |

```bash
eval "$(hack/kind/env.sh)"
kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" get nodes
```

`env.sh` exports `PATROL_CONTROL_KUBECONFIG`, `PATROL_CONTROL_CONTEXT`, `PATROL_TARGET_KUBECONFIG`,
`PATROL_TARGET_CONTEXT`, `PATROL_TARGET_INTERNAL_KUBECONFIG`, `PATROL_TARGET_INTERNAL_SERVER`,
`PATROL_REGISTRY` (`localhost:5001`), `PATROL_REGISTRY_INTERNAL` (`patrol-registry:5000`) and
`PATROL_UI_URL`. It does not export `KUBECONFIG`.

Every script goes through `hack/kind/lib.sh`, which refuses to run:

- kind against a cluster whose name is not `patrol-<name>`;
- kubectl without an explicit `--kubeconfig` file and `--context`, with a context not named
  `kind-patrol-<name>`, or with a context whose server is not a local kind endpoint
  (`https://127.0.0.1:*`, `https://localhost:*`, `https://[::1]:*` or
  `https://patrol-*-control-plane:6443`).

Scripts that need these checks source the library and call `patrol::kubectl <cluster> ...`.

## How patrol-control reaches patrol-target

Both clusters are kind clusters on the same Docker bridge network, `kind` (created by kind; all
kind clusters on the machine share it). Each node is a container whose name doubles as its DNS
name on that network, served by Docker's embedded DNS. The approach relies on kind's network
setup as follows:

- **Name resolution.** kind configures each node's resolver to use Docker's embedded DNS, and
  CoreDNS forwards non-cluster names to the node's resolver, so a pod in `patrol-control` resolves
  `patrol-target-control-plane` to that container's address on the `kind` network.
- **Routing.** Pod traffic leaves the `patrol-control` node container and enters the `kind`
  bridge; `patrol-target`'s API server listens on `:6443` on all interfaces inside its container.
  (The host reaches the same API server through a random `127.0.0.1` port mapping.)
- **TLS.** `kind get kubeconfig --internal` emits `https://patrol-target-control-plane:6443` with
  the cluster CA; kind's kubeadm configuration includes the control-plane container name in the
  API server certificate, so the connection verifies against that CA. `up.sh` writes this
  kubeconfig to `.cache/kind/patrol-target-internal.kubeconfig`.
- **Address.** The container's IP on the `kind` network is written to
  `.cache/kind/patrol-target-control-plane.ip` for diagnostics. Prefer the name: Docker may assign
  a different IP when the container restarts.
- **Distinct CIDRs.** `patrol-target` uses pod/service subnets `10.245.0.0/16`/`10.97.0.0/16`,
  `patrol-control` kind's defaults (`10.244.0.0/16`/`10.96.0.0/16`), so addresses in logs and
  resource status identify the cluster.

Checks: `up.sh` verifies the path from the `patrol-control` control-plane container;
`test/e2e/fixtures/smoke.sh` verifies it from a pod (`cross-cluster` check: DNS, TLS and an
authenticated `kubectl get nodes`). Status of these checks on this repository's reference
machine: [docs/verification/runs/kind-environment.md](../verification/runs/kind-environment.md).

### Register patrol-target with Patrol CD

Patrol CD reads destination clusters from `Cluster` objects (see
[CRD reference](../reference/crds.md#cluster)). With the CRDs installed in `patrol-control`:

```bash
hack/kind/register-target.sh --namespace <patrol-namespace> [--install-crds]
```

creates, in `patrol-target`, ServiceAccount `kube-system/patrol-manager` bound to
`cluster-admin` and a TokenRequest token for it (default validity 8760h, `--token-duration`),
and, in `patrol-control`, Secret `cluster-patrol-target` (keys `bearerToken`, `caData`, label
`cd.patrol.example/secret-type=cluster`) plus:

```yaml
apiVersion: cd.patrol.example/v1alpha1
kind: Cluster
metadata:
  name: patrol-target
spec:
  name: patrol-target
  server: https://patrol-target-control-plane:6443
  config:
    credentialsRef:
      name: cluster-patrol-target
```

(`hack/kind/manifests/cluster-patrol-target.yaml`). Patrol CD reads only Secrets that carry the
label `cd.patrol.example/secret-type` with the value of the kind that references them (`cluster`
for a Cluster, `repo-creds` for a RepositoryCredential, `repository` for a Repository; the write
credential types are in the [CRD reference](../reference/crds.md)). An unlabeled Secret counts
as missing: before the scripts set the label, every Application reported `ComparisonError:
credentials secret … not found` (finding KV-2 in `docs/verification/phase4-findings.md`). Label
Secrets you create by hand the same way. `--install-crds` applies `deploy/crds` server-side only
when they are not installed yet; otherwise the existing CRDs are left to their owner. Applications then target it with `destination.name: patrol-target`.

## Local registry

| From | Address | Notes |
|------|---------|-------|
| host | `localhost:5001` | `docker push`, `helm push --plain-http`, `oras push --plain-http` |
| pod image references | `localhost:5001/<image>` | containerd on every node maps it to the registry (`/etc/containerd/certs.d/localhost:5001/hosts.toml`) |
| pod image references | `patrol-registry:5000/<image>` | also mapped, plain HTTP |
| processes in pods (repo-server, helm, oras) | `patrol-registry:5000` | plain HTTP: `insecureOCIForceHTTP: true` on Repository objects, `--plain-http` for CLIs |

The registry is a container, not a kind port mapping: a host port can be bound once, and
`127.0.0.1:5001` belongs to `patrol-registry`. `patrol-control` maps only `127.0.0.1:8443` to
NodePort `30443`, reserved for the Patrol UI/API Service.

## Images

```bash
hack/kind/load-image.sh                          # every local patrol-cd/*:dev image -> both clusters
hack/kind/load-image.sh --cluster patrol-control patrol-cd/e2e-toolbox:dev
hack/kind/load-image.sh --registry patrol-cd/e2e-toolbox:dev   # -> localhost:5001/patrol-cd/e2e-toolbox:dev
```

Loaded images need `imagePullPolicy: IfNotPresent` or `Never` (the fixtures use `Never`, so a
missing image fails fast instead of being looked up on Docker Hub). When `kind load
docker-image` fails on Docker's containerd image store (multi-platform index with absent
platforms), the script retries with a single-platform `docker save --platform` archive.

## e2e fixtures

```bash
test/e2e/fixtures/deploy.sh        # build + load images, Secrets, git server, seed Job,
                                   # chart repo, toolbox, OCI artifacts (idempotent)
test/e2e/fixtures/smoke.sh         # in-cluster checks; --with-registry-image also pulls
                                   # localhost:5001/patrol-cd/e2e-toolbox:dev in both clusters
```

Endpoints, seeded repositories and refs, and credentials are listed in
[test/e2e/fixtures/README.md](../../test/e2e/fixtures/README.md). In short:

- git over smart HTTP with basic auth (`/git/`, push creates missing repositories), anonymous
  read-only (`/public/`), and SSH (`ssh://git@git-server.patrol-e2e.svc.cluster.local/git/…`) over
  the same repositories; the SSH host key is a committed **TEST-ONLY** key and
  `test/e2e/fixtures/git-server/known_hosts` lists it;
- repositories `guestbook`, `helm-guestbook`, `kustomize-guestbook`, `helm-values` and the
  monorepo `apps`, each with deterministic `v1.0.0`/`release-1.0`, `v1.1.0`/`main` and
  `feature/e2e` refs; re-running the seed Job resets them. When `examples/` exists, it is seeded
  too, as `patrol-examples.git` (plus `patrol-examples-values.git`), so the example Applications
  work after replacing their placeholder `repoURL` with
  `http://git-server.patrol-e2e.svc.cluster.local/git/patrol-examples.git`;
- a Helm repository (`helm-guestbook` 0.1.0 and 0.2.0, anonymous and basic auth);
- the same chart as an OCI artifact and the guestbook manifests as an OCI bundle in
  `patrol-registry`.

Credentials are generated per environment and stay in the cluster. To use them from the host:

```bash
kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" \
  -n patrol-e2e get secret git-server-http-auth -o go-template='{{.data.password | base64decode}}'
kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" \
  -n patrol-e2e port-forward svc/git-server 8080:80   # http://localhost:8080/git/<repo>.git
```

Debug from inside the cluster with the toolbox pod (`git`, `ssh`, `helm`, `oras`, `kubectl`):

```bash
kubectl --kubeconfig "$PATROL_CONTROL_KUBECONFIG" --context "$PATROL_CONTROL_CONTEXT" \
  -n patrol-e2e exec -it toolbox -- sh
```

### Register the fixtures with Patrol CD

```bash
test/e2e/fixtures/patrol/register.sh --namespace <patrol-namespace> [--with-applications]
```

copies the fixture credentials into Secrets `e2e-git-http-creds`, `e2e-git-ssh-creds` (label
`cd.patrol.example/secret-type=repo-creds`) and `e2e-chart-repo-creds` (label
`cd.patrol.example/secret-type=repository`) in the Patrol namespace and applies `patrol/repositories.yaml`
(credential templates for `/git/` over HTTP and SSH, the Helm repositories and the two OCI
repositories). `--with-applications` also applies `patrol/applications.yaml`: one manually
synced Application per source type (plain directory over HTTP and SSH, Helm from git, Kustomize
overlay, multi-source chart + `$values`, OCI chart, OCI manifests), all with
`destination.name: patrol-target`. For SSH, the script appends the lines of
`test/e2e/fixtures/git-server/known_hosts` (the TEST-ONLY host key) to the `ssh_known_hosts` key
of `patrol-ssh-known-hosts-cm` when they are missing, keeping the other entries.

### Webhooks

```bash
printf '%s' "$SECRET" > /tmp/webhook-secret   # the GitHub webhook secret configured in Patrol
test/e2e/fixtures/webhook/send-github-push.sh --secret-file /tmp/webhook-secret --insecure \
  --repo http://git-server.patrol-e2e.svc.cluster.local/git/guestbook.git \
  --ref refs/heads/main --changed guestbook-ui-deployment.yaml
```

The script signs the exact body it sends (`X-Hub-Signature-256: sha256=<HMAC-SHA256>`), posts
to `https://localhost:8443/api/webhook` by default (`--url`), and supports `--dry-run`,
`--payload FILE --event NAME` for other events and `--self-test` (GitHub's documented example).

### Fake GitHub API

Not available yet; see [test/e2e/fixtures/fake-github/README.md](../../test/e2e/fixtures/fake-github/README.md).

## Offline checks

```bash
test/e2e/fixtures/validate.sh
test/e2e/fixtures/test-scripts.sh
```

starts the envtest `etcd` and `kube-apiserver` (Kubernetes 1.36, `bin/envtest`, `make envtest`),
installs `deploy/crds`, and dry-run creates every fixture manifest, the Patrol objects in
`test/e2e/fixtures/patrol/` and `hack/kind/manifests/`, and the rendered seed applications
(`kustomize build`, `helm lint --strict` + `helm template` with each values file).
`test-scripts.sh` runs two such API servers with kubeconfig contexts named
`kind-patrol-control`/`kind-patrol-target` and exercises the context guards,
`hack/kind/register-target.sh` and `test/e2e/fixtures/patrol/register.sh` against them. Neither
needs Docker or network access.

## Tear down

```bash
make kind-down           # = hack/kind/down.sh
```

deletes every kind cluster named `patrol-*`, the `patrol-registry` container (only if it carries
the hack/kind label) with its storage volume, and the kubeconfigs in `.cache/kind/`, then
verifies that none remain. The `kind` network, other kind clusters and images are kept;
`docker image rm` the `patrol-cd/*:dev` images yourself if you want the space back.

## Slow machines and CI

The GitHub Actions workflow and the `make ci-*` targets that mirror its jobs are described in
[ci.md](ci.md).

The waits in the kind and fixture scripts were tuned on an 8-CPU workstation. On a slower machine,
such as a 2-vCPU CI runner, give every wait more time with `PATROL_WAIT_SCALE`:

```bash
PATROL_WAIT_SCALE=3 make kind-up kind-fixtures kind-install kind-verify
```

- **What scales.** Every wait and timeout of `hack/kind/*.sh` and `test/e2e/fixtures/*.sh` is
  multiplied by it:
  - `kind create cluster --wait`;
  - the node, CoreDNS, rollout, Job and Pod waits (`kubectl --timeout`);
  - the `helm --timeout` of `install.sh` and of the `--lifecycle` section;
  - the polling limits of `verify-install.sh`;
  - the host-key wait and the CRD wait;
  - the git-seed Job's deadline and its wait for the git server;
  - the repo-server connection checks (`nc -w`) and the sync request's time limit.

  Poll intervals and the short limits of single API calls (`curl -m`, guards against a hung
  connection) stay as they are.
- **Range.** An integer from 1 to 10. Unset or empty means 1, which keeps the values in the
  scripts. Any other value stops the script before it does anything.
- **What a check accepts.** A larger value lets a wait run longer before it gives up. The
  condition checked stays the same: for example, `verify-install.sh` still requires every Patrol
  pod to have 0 restarts. But a check that something happens within a window also accepts the
  slower path. For example, at `PATROL_WAIT_SCALE` >= 2 the `drift` window exceeds the
  controller's periodic refresh (120 s + up to 60 s jitter), so it also passes when only the
  periodic refresh reports the drift. Watch-driven refresh is covered by
  `TestRefreshAnnotationAndRecentRefresh` in `internal/controller/integration`, which runs in the
  unit job.
- **Shared deadlines.** `verify-install.sh` waits for the fixture Applications to compare, and
  later for their sync operations, with one deadline each (150 s and 180 s, scaled) shared by all
  seven Applications, because the controller works on them concurrently. A polled condition is
  always checked at least once, also when the deadline has passed. When an Application has not
  compared by the deadline (or the number of fixture Applications is not 7), the run stops before
  it sends any sync request.
- **CI.** The kind job of the GitHub Actions workflow (`.github/workflows/ci.yml`) sets it.

**Pinned images.** Like the kind node image, the registry image (`registry:3.0.0`,
`PATROL_REGISTRY_IMAGE` in `hack/kind/lib.sh`) and the toolbox's `oras` build stage
(`ghcr.io/oras-project/oras:v1.3.4`, `test/e2e/fixtures/toolbox/Dockerfile`) are pinned to the
digest of their multi-platform index. To refresh a pin or move to a new tag, read the index digest
(the `Digest:` line) and check that `linux/amd64` and `linux/arm64` are listed:

```bash
docker buildx imagetools inspect registry:3.0.0
docker buildx imagetools inspect ghcr.io/oras-project/oras:v1.3.4
```

Then put `<name>:<tag>@<digest>` in that file and in the `docker pull` list of
[test/e2e/fixtures/README.md](../../test/e2e/fixtures/README.md#network-and-offline-use).
`test/e2e/fixtures/test-scripts.sh` checks that the references stay digest-pinned.

**Pull retries.** When the node or registry image is missing locally, `up.sh` pulls it itself
before `kind create cluster` or `docker run`. It makes up to 3 attempts, 5 s and then 15 s apart
(this backoff does not scale), so a transient Docker Hub error (429 rate limit, 5xx) is retried.
If the last attempt fails, the error names the image. Images already present are not pulled again.

## Troubleshooting

| Symptom | Cause / fix |
|---------|-------------|
| `no space left on device` from `docker run`, `kind create cluster` or `docker build` | The Docker VM disk is full. Inspect with `docker system df`; free space (unused images, build cache, volumes) or enlarge the Docker Desktop disk. The scripts never prune anything. |
| `apk add` fails during `build-images.sh` / `deploy.sh` (e.g. `temporary error`, `unable to select packages`) | The fixture build needs the Alpine package mirrors on its first run or after the build cache was pruned. Build once with network access, or reuse built images with `deploy.sh --skip-build`. |
| `refusing to operate on kube context ...` | A script was pointed at a non-`kind-patrol-*` context or a kubeconfig whose server is not local. Use the files in `.cache/kind/`. |
| `a container named patrol-registry exists but was not created by hack/kind` | Remove or rename that container; `up.sh` and `down.sh` do not touch unlabelled containers. |
| `ErrImageNeverPull` on fixture pods | Images were not loaded into `patrol-control`: run `test/e2e/fixtures/deploy.sh` (or `hack/kind/load-image.sh --cluster patrol-control ...`). |
| `cross-cluster` smoke check fails on name resolution | Check `getent hosts patrol-target-control-plane` in the toolbox pod and `docker network inspect kind`; both node containers must be on the `kind` network. |
| Port 8443 or 5001 already in use | Stop the other listener; the ports are fixed by the cluster config and the registry. |
| Seeded repositories gone after the git server pod restarted | Repositories live in an `emptyDir`: re-run `test/e2e/fixtures/deploy.sh --skip-build`. |
