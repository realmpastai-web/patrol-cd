# Installing Patrol CD with Helm

The chart in `deploy/helm/patrol-cd` installs every Patrol CD component from the single
`patrol-cd/patrol-cd` image (see [images.md](images.md)). Its values are documented in the
generated [chart README](../../deploy/helm/patrol-cd/README.md); this guide explains how to use
them. Installations without Helm render the same chart (see [Without Helm](#without-helm)).

Status: the chart renders, lints and validates statically (`make helm-lint helm-template`,
recorded in `docs/verification/runs/2026-10-17-chart-static.md`) and is installed, upgraded,
rolled back, uninstalled and re-installed against envtest API servers (`test/deploy`,
`docs/verification/runs/2026-10-17-deploy-validation.md`). Its default profile was installed on
a local kind cluster with a kubelet on 2026-10-19, with a second kind cluster as the destination,
and checked by `hack/kind/verify-install.sh`
(`make kind-install kind-verify`; `docs/verification/runs/2026-10-19-kind-validation.md`), with
a locally built image (`docs/verification/runs/2026-10-19-image-build.md`). Not run on a
cluster: the other profiles, more than one replica, other CNIs than kindnet, `linux/amd64`
nodes and any cloud cluster.

## Prerequisites

| Requirement | Notes |
|-------------|-------|
| Kubernetes `>= 1.30` | ValidatingAdmissionPolicy (shipped with the CRDs) is GA from 1.30. The Argo CD v3.5.5 baseline is tested on 1.33–1.36. |
| Helm 4 (checked with v4.3.1; Helm 3 untested) | Helm 4 applies with server-side apply; the ApplicationSet CRD (1.4 MB YAML) is too large for client-side `kubectl apply`, not for Helm. |
| The Patrol image in a registry your nodes can pull | Nothing is published yet: build it (`make image`, [images.md](images.md)) and push it to your registry, then set `image.repository`/`image.tag` (and `image.digest`). The default tag is the chart's appVersion, `0.1.0`; images tagged `v0.1.0` (the image guide's examples) or `dev` (local builds, `make image`) need `image.tag`. |
| A namespace per installation | Settings objects have fixed names (`patrol-cm`, `patrol-secret`, …), so one release per namespace. |
| Optional | cert-manager (internal TLS, server certificate), Prometheus Operator (ServiceMonitor, PrometheusRule), Gateway API (HTTPRoute), an Ingress controller. |
| Cluster permissions for the installer | The default install creates CRDs, a ValidatingAdmissionPolicy and ClusterRoles: cluster-admin. The namespaced profile (CRDs installed by a cluster administrator) creates Roles only, but a namespace `admin` cannot install it: RBAC escalation prevention refuses Roles that grant permissions the installer does not hold (the Patrol CRD verbs, and the namespace-wide `*` rules of the controller and patrol-server). Install it as a user bound to the `cluster-admin` ClusterRole by a RoleBinding in the namespace (full rights in that namespace only), or as a namespace `admin` who also holds `escalate` and `bind` on Roles of the namespace. Both are checked by `TestNamespaceInstallPermissions` (`test/deploy`). |

## Install

From a checkout (the chart is not published to a chart repository or OCI registry):

```bash
helm upgrade --install patrol deploy/helm/patrol-cd \
  --namespace patrol --create-namespace \
  --set image.repository=registry.example.com/patrol-cd/patrol-cd \
  --set image.tag=0.1.0 \
  --wait --timeout 10m
```

From a packaged archive: `make helm-package` writes `dist/patrol-cd-0.1.0.tgz`; install it
with `helm upgrade --install patrol dist/patrol-cd-0.1.0.tgz …`. To serve it from your own OCI
registry, `helm push dist/patrol-cd-0.1.0.tgz oci://registry.example.com/charts` (your
registry, your credentials) and install `oci://registry.example.com/charts/patrol-cd
--version 0.1.0`.

What happens on the first install:

1. Helm applies the CRDs, the Operation admission policy, the settings ConfigMaps, an empty
   `patrol-secret`, RBAC, Services and workloads. The server, the controller, the ApplicationSet
   controller and the notifications controller wait up to a minute for the API server to serve every
   Patrol kind before they start (a large CRD can lag behind the pods on a busy cluster); the log
   says `waiting for the Patrol CRDs to be served`. Past that minute they exit with the missing kinds
   in the error and the kubelet restarts them.
2. The `bootstrap` init container of patrol-server (of the controller in a core install) runs
   `patrolcd bootstrap`: it creates `patrol-internal-token`, the session signing key in
   `patrol-secret` and the initial admin password (`patrol-secret` `admin.password`, plaintext in
   `patrol-initial-admin-secret`), and seeds the default SSH known hosts into
   `patrol-ssh-known-hosts-cm` (see [Trust material](#trust-material)). It never overwrites
   existing material and is safe with several replicas.
3. Pods that mount the internal token (repo-server, ApplicationSet and notifications
   controllers, commit-server, and the controller unless it runs the bootstrap) stay in
   `ContainerCreating` until the Secret exists; the kubelet retries the mount (up to about two
   minutes). Only the pod that runs the bootstrap (patrol-server, or the controller in a core
   install) mounts it as optional, and reads the Secret through the API while the mount is
   still empty.

No template renders random values and the chart never uses `lookup`, so `helm template` output
is deterministic and a GitOps tool can render it.

## First login

```bash
patrol admin initial-password -n patrol          # prints the generated password
kubectl -n patrol port-forward service/patrol-server 8080:443
patrol login localhost:8080 --username admin --insecure   # self-signed certificate
patrol account update-password
kubectl -n patrol delete secret patrol-initial-admin-secret
```

Set `configs.cm.url` to the external URL before you use SSO, notifications or links.

Alternatives: `configs.secret.adminPasswordHash` (bcrypt, from `patrol account bcrypt`; Helm
then owns the hash and reverts UI password changes on upgrade), or
`configs.secret.createAdmin: false` with `configs.cm."admin.enabled": "false"` for SSO-only
installations.

## Configuration model

| Values | Object | Notes |
|--------|--------|-------|
| `configs.cm` | `patrol-cm` | Keys validated against the settings register (`internal/settings`); a typo fails `helm install`. The shipped resource exclusions and `ignoreResourceUpdates` rules equal the code's defaults (`make helm-verify`). Followed live, no restart. |
| `configs.params` | `patrol-cmd-params-cm` | Closed key set (keys without effect in Patrol, such as `redis.*`, are rejected). Read at startup: pods roll when it changes (checksum annotation). Keys the chart drives (`application.namespaces`, `hydrator.enabled`, `commit.server`, internal TLS keys, `server.insecure`) are set through structured values. |
| `configs.rbac` | `patrol-rbac-cm` | `policy.csv`, `policy.<name>.csv`, `policy.default`, `policy.matchMode`, `scopes`. |
| `configs.tlsCerts`, `configs.sshKnownHosts`, `configs.gpgKeys` | trust ConfigMaps | Mounted into the repo-server and commit-server. Leave them empty to manage trust through the API and CLI; entries set here are owned by Helm ([Trust material](#trust-material)). |
| `configs.secret.extra` | `patrol-secret` | Prefer `$<secret>:<key>` references to Secrets labelled `app.kubernetes.io/part-of: patrol-cd`. |
| `notificationsController.cm` / `.secret` | `patrol-notifications-cm` / `-secret` | Credentials belong in the Secret (preferably managed outside Helm: `secret.create: false`). |

Set `configs.create.<object>: false` (or `configs.secret.create`, `notificationsController.
createConfigMap`) when another tool owns an object. Names are fixed; `fullnameOverride` only
changes the `patrol-` prefix of workloads, Services and ServiceAccounts, and cluster-scoped
objects are named `<namespace>-<prefix>-<component>`.

## Trust material

Known hosts, TLS certificates and GnuPG keys can be managed in two ways. Pick one per object
(per key for TLS certificates and GnuPG keys): Helm 4 upgrades with server-side apply and owns
every field it renders, while the API (`patrol cert add-ssh`/`add-tls`/`rm`, `patrol gpg add`/`rm`,
the UI) rewrites the same ConfigMaps with plain updates.

| Object | Managed through the API (default) | Managed through values |
|--------|-----------------------------------|------------------------|
| `patrol-ssh-known-hosts-cm` | `configs.sshKnownHosts: ""`: the chart renders the ConfigMap without data; the bootstrap writes the published host keys of GitHub, Bitbucket Cloud, GitLab.com and Azure DevOps once (only while the key is absent, so hosts removed through the API stay removed). Upgrades, rollbacks and bootstrap re-runs keep API changes. | `configs.sshKnownHosts: <known_hosts lines>`: Helm owns the whole key and the bootstrap leaves it alone (`--skip-ssh-known-hosts`). An API change to known hosts then makes the next `helm upgrade` fail with `conflict with "<manager>" using v1: .data.ssh_known_hosts`; `--force-conflicts` succeeds and reverts the change. |
| `patrol-tls-certs-cm`, `patrol-gpg-keys-cm` | Empty `configs.tlsCerts` / `configs.gpgKeys`: Helm owns only the metadata; entries added through the API survive upgrades. | Entries in `configs.tlsCerts` / `configs.gpgKeys` are owned by Helm; an API change to the same host name or key ID conflicts as above. API entries under other names are kept. |

Switching known hosts from API-managed to values-managed needs one `helm upgrade
--force-conflicts` (the key is owned by the bootstrap or patrol-server until then). Switching
back (an upgrade whose values leave `configs.sshKnownHosts` empty; note that `helm upgrade`
without any values reuses the previous release's values unless `--reset-values` is given)
removes the key, and the bootstrap of the restarted patrol-server pods (their arguments change)
writes the defaults again. The repo-server and commit-server read the
mounted file, which the kubelet refreshes within about a minute of a change (so does the first
seeding after a fresh install). With `bootstrap.enabled: false` or `configs.create.sshKnownHosts:
false`, nothing seeds the defaults. Verified by `TestTrustConfigMapsHelmUpgrade` (`test/deploy`)
and `TestBootstrapSeedsKnownHosts` (`internal/server`).

## Profiles

`deploy/helm/patrol-cd/ci/` holds tested value sets: `minimal`, `default`, `hardened`, `ha`,
`core`, `hydrator`, `airgap`, `namespaced`, `ingress-nginx` and `all-features` (every optional
path, not a recommendation). Combine them with your own values: `-f ci/ha.yaml -f mine.yaml`.

Core (headless) install, no API server, UI or Dex:

```bash
helm upgrade --install patrol deploy/helm/patrol-cd -n patrol --create-namespace -f deploy/helm/patrol-cd/ci/core.yaml
patrol app list --core -n patrol
```

A core installation has no patrol-server, and only patrol-server creates the AppProject
`default`. Create it (or your own project) before the first Application
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

## Exposing the API and UI

patrol-server serves the API, UI and webhooks on one port (8080), HTTPS by default. Its
Service has two ports, `http` (80) and `https` (443), both to 8080.

TLS terminated by the Ingress controller (recommended):

```yaml
configs:
  cm:
    url: https://patrol.example.com
server:
  insecure: true          # plain HTTP behind the controller (--insecure)
  ingress:
    enabled: true
    className: nginx
    annotations:
      nginx.ingress.kubernetes.io/backend-protocol: HTTP
      nginx.ingress.kubernetes.io/force-ssl-redirect: "true"
      nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"   # log streams, terminal
    hosts: [patrol.example.com]
    tls:
      - secretName: patrol-ingress-tls
        hosts: [patrol.example.com]
networkPolicy:
  serverFrom:              # only the ingress controller may reach the API port
    - namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: ingress-nginx}}
      podSelector: {matchLabels: {app.kubernetes.io/name: ingress-nginx}}
```

TLS at patrol-server (re-encryption or passthrough at the proxy): keep `server.insecure:
false`, use the `https` backend port (the chart selects it), set the controller's backend
protocol to HTTPS (`nginx.ingress.kubernetes.io/backend-protocol: HTTPS`, or
`ssl-passthrough: "true"`), and provide the certificate:

- `server.tls.existingSecret: <kubernetes.io/tls Secret>` (mounted, reloaded on rotation), or
- `server.certificate.enabled: true` with `dnsNames` and an `issuerRef` (cert-manager writes
  `patrol-server-tls`), or
- nothing: patrol-server uses `patrol-secret` `tls.crt`/`tls.key` when present, else a
  self-signed certificate generated in memory at each start.

Gateway API: `server.httproute.enabled: true` with `parentRefs` and `hostnames` (needs
`gateway.networking.k8s.io/v1`). Terminate TLS at the Gateway and set `server.insecure: true`;
with TLS at the server the Gateway also needs a BackendTLSPolicy (not rendered by the chart).

The CLI talks plain HTTP/JSON; no separate gRPC host or gRPC-Web routing is needed.

## Single sign-on

External OIDC provider (preferred): put the client secret into `patrol-secret` or a labelled
Secret and reference it.

```yaml
configs:
  cm:
    url: https://patrol.example.com
    oidc.config: |
      name: Okta
      issuer: https://example.okta.com
      clientID: patrol
      clientSecret: $oidc-okta:clientSecret
      requestedScopes: [openid, profile, email, groups]
  rbac:
    policy.csv: |
      g, platform-admins, role:admin
    scopes: "[groups]"
```

Bundled Dex (`dex.enabled: true`) runs the upstream image (`ghcr.io/dexidp/dex:v2.45.1`, pinned
by digest) behind patrol-server's `/api/dex` proxy. Argo CD generates Dex's configuration from
`argocd-cm` with its `argocd-dex` wrapper; Patrol has no such wrapper (OPS-152), so:

1. Set `configs.cm.url` and `configs.cm."dex.config"` (the connectors; patrol-server reads it to
   enable Dex login and list the connectors).
2. Put the connectors into `dex.config` (merged over the chart's issuer, storage, listeners and
   the `patrol`/`patrol-cli` static clients) with credentials as `$ENV_VAR` references supplied
   through `dex.env`/`dex.envFrom`, or provide a complete configuration in
   `dex.existingConfigSecret` (key `config.yaml`).
3. Give Dex the client secret patrol-server expects for the `patrol` client. It is derived from
   the session signing key, so create it after the first install:

   ```bash
   hexkey=$(kubectl -n patrol get secret patrol-secret -o jsonpath='{.data.server\.secretkey}' | base64 -d | xxd -p | tr -d '\n')
   secret=$(printf '%s' 'patrol dex client secret v1' | openssl dgst -sha256 -mac HMAC -macopt "hexkey:$hexkey" -binary | base64 | tr '+/' '-_' | cut -c1-40)
   kubectl -n patrol create secret generic patrol-dex-client --from-literal=clientSecret="$secret"
   ```

   and set `dex.clientSecretRef.name: patrol-dex-client`. Recreate it if `server.secretkey` is
   rotated. (The derivation is `base64url(HMAC-SHA256(server.secretkey, "patrol dex client
   secret v1"))[:40]`, `internal/auth/oidc/dex.go`; the pipeline above was checked against it.)

Dex listens on plain HTTP inside the cluster (`server.dex.server.plaintext`); its NetworkPolicy
admits only patrol-server. Dex uses memory storage (no Kubernetes access): sessions do not
survive a Dex restart, and it runs one replica.

Patrol refuses the IdP's whole key set (JWKS) if it holds an RSA key above 4096 bits, or an
Ed25519 key with the wrong length or a low-order point, even when that key did not sign the
token. go-jose enforces this while parsing the set, and no Patrol setting changes it. Argo CD
v3.5.5 has an older go-jose and accepts these key sets. Discovery does not read the keys, so the
failure appears when Patrol has to fetch them. That happens at the first verification after a
start, and it can happen at any later time, so a running server that still verifies tokens is no
proof that a key set is safe.

| Path | What you see |
|------|--------------|
| Web login | The user signs in at the IdP, then gets a 401 `ID token verification failed`; patrol-server logs `SSO login failed` with go-jose's message in `error` |
| `patrol login --sso` | The command fails with go-jose's message |
| A `patrol` command whose SSO session has expired | The renewal fails with go-jose's message; `patrol account session-token` prints `Refresh token is invalid or expired` instead. `patrol relogin` fails with go-jose's message until the key set is fixed. Patrol redeems the refresh token before it verifies the ID token, so on an IdP that rotates refresh tokens the next renewal fails with `invalid_grant`, even after the fix, until you run `patrol relogin` |
| ID token as a bearer token | Routes that need a login answer 401 `invalid session: invalid token`, and public routes treat the caller as not logged in; no reason is logged |

go-jose's message is one of:

- `go-jose/go-jose: invalid RSA key, modulus too large`
- `go-jose/go-jose: invalid Ed25519 public key, wrong length for x`
- `go-jose/go-jose: invalid Ed25519 public key, low-order point`

The fix is on the IdP side: remove or replace that key. Local accounts are not affected, and
Patrol sessions already issued stay valid until they expire.

## Security model and RBAC

| Component | Kubernetes access (release namespace) | Local cluster |
|-----------|---------------------------------------|---------------|
| patrol-server | Patrol CRDs; every Secret (get, list, watch, create, update, delete) and ConfigMap (get, list, watch, create, update) of the namespace: settings, credentials, tokens; Events; Lease get; Operations with `impersonate` | `rbac.server.localCluster` (default on): get/patch/delete any resource, list Events, read pods and logs, create Jobs; `pods/exec` with `rbac.server.exec`; ServiceAccount `impersonate` with `rbac.server.impersonation` |
| controller | get, list and watch every Secret and ConfigMap of the namespace (settings, cluster and repository credentials); shard Leases; Patrol CRDs; Application and Operation writes and status; Events | `rbac.controller.clusterAdmin` (default on): every resource and verb |
| repo-server, commit-server, Dex | none (no token mounted) | none |
| ApplicationSet controller | get, list and watch every Secret and ConfigMap of the namespace (settings, repository and SCM credentials); ApplicationSets, Applications, Operations (create and delete with `impersonate`); leader Lease; Events | none |
| notifications controller | list and watch every ConfigMap and Secret of the namespace (settings informers and the repository credential lister; list and watch return whole objects, so this covers `patrol-secret` with `server.secretkey`, `patrol-internal-token` and every cluster and repository credential); get `patrol-notifications-cm`, `patrol-cmd-params-cm` and `patrol-notifications-secret`; read Applications, Operations, projects, repositories and SCM providers; patch (annotate) Applications; Events; leader Lease. With self-service notifications: get, list and watch `patrol-notifications-cm` and `patrol-notifications-secret`, by name, in Application namespaces | none |
| bootstrap (init container) | create Secrets, get its three Secrets, update `patrol-secret`, delete `patrol-initial-admin-secret` (only that Secret, by name: a stale one is replaced, see [Uninstall](#uninstall)), get `patrol-cm`, get/update `patrol-ssh-known-hosts-cm` (when it seeds the known hosts) | none |

Consequences to know:

- **The controller is cluster-admin on the local cluster by default** (as Argo CD): anyone who
  may create an Application in a project that allows the in-cluster destination can deploy any
  resource, including RBAC, to this cluster. Restrict the `default` project, or set
  `rbac.controller.clusterAdmin: false` and list the allowed writes in `rbac.controller.rules`
  (the default rules read everything, which the live-state cache needs; add `impersonate` on
  ServiceAccounts if you use sync impersonation).
- **patrol-server can read every resource of the local cluster, Secrets included** (needed for
  live views of the in-cluster destination; the API masks Secret data). Turn it off with
  `rbac.server.localCluster: false` if you do not deploy to the local cluster.
- **patrol-server, the controller and the ApplicationSet and notifications controllers can
  read every Secret of the release namespace**, including `server.secretkey` (signs sessions)
  and the internal token. Keep other workloads and people out of that namespace.
- Only patrol-server, the controller and the ApplicationSet controller may create Operations on
  behalf of others (`impersonate` verb checked by the admission policy).
- With `<component>.serviceAccount.create: false`, `<component>.serviceAccount.name` is required:
  the chart binds the component's RBAC to that existing ServiceAccount and refuses to render
  rather than bind it (cluster-admin for the controller) to the namespace's `default`
  ServiceAccount, which every pod without `serviceAccountName` uses.
- Namespaced mode (`rbac.scope: namespaced`, profile `ci/namespaced.yaml`) creates Roles only:
  the controller manages the release namespace and `rbac.managedNamespaces` on the local cluster,
  CRDs and the admission policy are installed separately (`crds.install: false`), and
  `applicationNamespaces` must be empty. Applications that target the local cluster need a
  Cluster object for `https://kubernetes.default.svc` whose `namespaces` list the release
  namespace and `rbac.managedNamespaces` (example in
  [manifests.md](manifests.md#install-a-namespace-scoped-variant)); without it the controller
  lists resources cluster-wide, which its Roles refuse, and comparisons fail. The installer
  needs more than the namespace `admin` role (see
  [Prerequisites](#prerequisites)), in the release namespace and in every
  `rbac.managedNamespaces` entry.
- Applications in other namespaces (`applicationNamespaces`, `applicationSetNamespaces`): literal
  names get Roles in those namespaces (the controller's rules are cluster-wide because it watches
  all namespaces once any is configured); globs or `/regexp/` make the rules cluster-wide.

All pods run as uid/gid 999 (Dex: 1001, its image user), non-root, read-only root filesystem, no
privilege escalation, all capabilities dropped, seccomp `RuntimeDefault` (Pod Security
`restricted`); writable paths are
size-limited `emptyDir` volumes; ServiceAccounts do not automount tokens and only the components
that call the API mount one.

## NetworkPolicies

With `networkPolicy.enabled` (default) every component's pods accept only their documented
callers and reach only their documented destinations:

| Component | Ingress | Egress |
|-----------|---------|--------|
| server | 8080 from `serverFrom` (default anyone); 8083 from `metricsFrom` | DNS, repo-server 8081, controller 8090, Dex 5556/5557, API server, `serverEgress` (IdPs, remote clusters, extension backends), OTLP collector |
| controller | 8090 from server; 8082 from `metricsFrom` | DNS, repo-server 8081, commit-server 8086, API server, `controllerEgress` (destination clusters), OTLP collector |
| repo-server | 8081 from server, controller, ApplicationSet and notifications controllers; 8084 from `metricsFrom` | DNS, `repoServerEgress` (Git/Helm/OCI: 22, 80, 443) minus the cloud metadata endpoints, OTLP collector |
| ApplicationSet controller | 8080 from `metricsFrom`; webhook port when enabled | DNS, repo-server, API server, `appsetControllerEgress` (SCM APIs) |
| notifications controller | 9001 from `metricsFrom` | DNS, repo-server, API server, `notificationsEgress` (80, 443, 465, 587) |
| commit-server | 8086 from controller; 8087 from `metricsFrom` | DNS, `commitServerEgress` (Git) minus the cloud metadata endpoints |
| Dex | 5556/5557 from server; 5558 from `metricsFrom` | DNS, `dexEgress` (IdPs) |

Tighten with the `*Egress.cidrs`/`except`/`ports` values (`ci/hardened.yaml`) and
`networkPolicy.extraEgress.<component>`. Notes:

- OTLP collector egress exists only while an OTLP address is configured (`tracing.otlpAddress`,
  or `otlp.address` in `configs.params`): TCP to the address's port (4317 when it has none, or
  `tracing.networkPolicy.port`), to any destination unless `tracing.networkPolicy.to` lists the
  collector's pods or CIDRs.
- DNS egress targets `kube-dns` pods in `kube-system`; adjust `networkPolicy.dns.to` for
  NodeLocal DNSCache (add an `ipBlock` for its link-local address) or other DNS setups.
- The API server is matched by IP (`networkPolicy.kubeAPIServer.cidrs`, default any address on
  443/6443). Narrow it to the endpoints of the `kubernetes` Service. Some CNIs (Cilium) need
  their own construct for the API server (`toEntities: kube-apiserver`) instead of an `ipBlock`.
- Kubelet probes come from the node and are allowed by most CNIs regardless of policies.
- **Ports are the destination pod's ports.** NetworkPolicy matches a connection after Service
  translation, so `ports` must list the port the target pod listens on, not the Service port. An
  in-cluster Git server, chart repository or registry that listens on 8080 behind a Service on
  port 80 needs 8080 in `repoServerEgress.ports` (the defaults are 22, 80 and 443); with the
  default ports the repo-server's requests to it hang until the exec timeout (90 s for git) and
  the Applications report a comparison error. This was seen on a kind cluster (kindnet), where
  `ipBlock` `0.0.0.0/0` does match pod IPs, so only the port blocked the connection; other CNIs
  differ in whether an `ipBlock` matches pod IPs at all, and then a `namespaceSelector` entry in
  `networkPolicy.extraEgress.repoServer` is needed as well.
- `except` entries: each one is removed from every CIDR of the same `*Egress.cidrs` list that
  strictly contains it, and ignored for the others (the API server rejects an `except` outside
  its `cidr`). In-cluster Git servers, registries or IdPs fall under the `0.0.0.0/0` CIDRs only
  where the CNI matches pod IPs with `ipBlock` (kindnet does), and they are reached only on the
  ports their pods listen on (see the bullet above: with the default ports 22, 80 and 443 a
  server whose pod listens on 8080 behind a Service on 80 stays unreachable). When you add
  `except` for private ranges, add their CIDRs back in the same list:
  `cidrs: [0.0.0.0/0, 10.20.0.0/16]` with `except: [10.0.0.0/8]` allows 10.20.0.0/16 and the
  public addresses. CIDRs must be canonical `address/prefix` strings (schema-checked).
- The repo-server and the commit-server fetch tenant-supplied URLs, so their external egress
  excludes the cloud instance metadata endpoints by default
  (`networkPolicy.excludeMetadataEndpoints`: `169.254.169.254/32` for AWS, GCP and Azure,
  `fd00:ec2::254/128` for AWS over IPv6). Set it to `[]` if these components must reach a
  metadata server, for example for workload identity credentials served by it. Other
  components keep their egress unchanged (the controller and patrol-server may get cloud
  credentials for destination clusters, `awsAuthConfig` or exec plugins, from a metadata
  server).
- Enforcement depends on the CNI; it has not been tested on a cluster yet.

## Internal TLS

| `internalTLS.mode` | repo-server | commit-server, controller tree API | Verification |
|--------------------|-------------|------------------------------------|--------------|
| `disabled` | plaintext (`--disable-tls`) | plaintext | none; the internal token authenticates |
| `selfSigned` (default) | TLS with an in-memory certificate (or `patrol-repo-server-tls` if you create it) | plaintext | none (clients skip verification, as Argo CD) |
| `certManager` | TLS | TLS (commit-server reached as `https://`) | clients verify the CA (`ca.crt`) and host name with `verify: true`; `mtls: true` makes the repo-server and commit-server require client certificates |
| `existingSecret` | as certManager with Secrets you create: `patrol-repo-server-tls`, `patrol-commit-server-tls`, `patrol-controller-tls` (each with `ca.crt`), and `patrol-internal-client-tls` for mTLS | | |

With cert-manager and no `internalTLS.certManager.issuerRef`, the chart creates a self-signed
root and a CA Issuer in the release namespace. The tree API certificate covers
`*.<prefix>-controller.<namespace>.svc` (patrol-server dials the replica holding the shard).
The tree API cannot require client certificates (patrol-server has no client certificate for
it); the internal token still authenticates every call.

## High availability

`ci/ha.yaml`: three controller shards (consistent hashing), autoscaled server (3–6) and
repo-server (3–10), two ApplicationSet and notifications replicas (leader-elected), hard
anti-affinity per node (at least three schedulable nodes) and zone spread, PDBs
(`maxUnavailable: 1`), cert-manager internal TLS with mTLS, ServiceMonitors and alerts.

- Controller shards are coordinated by Leases (`patrol-controller-shard-<n>`); change the
  number of shards with `controller.replicas` and `helm upgrade`. Without dynamic cluster
  distribution the count is also in `PATROL_CONTROLLER_REPLICAS`, so the change restarts every
  shard; with `controller.sharding.dynamicClusterDistribution: true` the pod template does not
  carry it, only the StatefulSet's `spec.replicas` changes, and the running replicas rebalance
  without restarting. Do not use `kubectl scale` on a Helm release: Helm owns `spec.replicas`,
  and the next `helm upgrade` fails with `conflict with "kubectl" with subresource "scale" using
  apps/v1: .spec.replicas` (`--force-conflicts` succeeds and resets it to `controller.replicas`).
  Checked by `TestControllerReplicasHelm` (`test/deploy`).
- The controller StatefulSet uses `podManagementPolicy: Parallel` (shard Leases arbitrate
  ownership). The field is immutable on a StatefulSet: changing `controller.podManagementPolicy`
  on an installed release makes `helm upgrade` fail until the StatefulSet is deleted with
  `kubectl -n patrol delete statefulset patrol-controller --cascade=orphan` and re-created by
  the upgrade.
- PodDisruptionBudgets: `<component>.pdb.maxUnavailable` (default 1) or `minAvailable`. An
  explicit `maxUnavailable: 0` is rendered as 0 and blocks every voluntary eviction: node drains
  and cluster upgrades wait until it is changed.
- HPAs (`server.autoscaling`, `repoServer.autoscaling`) need a CPU or memory target above 0 and
  `minReplicas` not above `maxReplicas`; the chart refuses other values (an HPA without metrics
  would scale on 80% CPU).
- The repo-server is stateless; each replica keeps its own render cache (no Redis, ADR-0004).
- Sizing: start from the profile's requests and watch `patrol_app_reconcile_duration_seconds`,
  `patrol_reposerver_parallelism_wait_seconds` and memory; the controller's memory grows with
  the number of live objects cached per destination cluster.

## Hardened installation

`ci/hardened.yaml` shows the knobs: SSO-only (`admin.enabled: "false"`, no generated admin
password), no anonymous access, terminal disabled, verified internal mTLS with your own
certificates, `rbac.server.localCluster: false`, controller writes limited to listed kinds,
metrics scraping only from Prometheus, egress narrowed to public addresses on the needed
ports and the API server endpoints. Also consider `global.priorityClassName`, Pod Security
admission `enforce: restricted` on the namespace (the chart complies), and an image digest
(`image.digest`).

What the controller rules of that profile prevent, and what they do not:

- They name the writable resources (ConfigMaps, Secrets, Services, ServiceAccounts, PVCs, Pods,
  Deployments, StatefulSets, DaemonSets, Jobs, CronJobs, Ingresses, NetworkPolicies, HPAs,
  PDBs) instead of `*`, so the controller cannot create ServiceAccount tokens
  (`serviceaccounts/token`), exec or attach into pods, add ephemeral containers, proxy to
  nodes (`nodes/proxy` create), or change nodes, namespaces, RBAC or admission objects.
- They do not contain the controller. The read rule the live-state cache needs (get, list,
  watch on every resource) covers every Secret of the cluster and every subresource readable
  with `get`, `nodes/proxy` included. Writing Pods, workloads, Secrets or ConfigMaps in any
  namespace, `kube-system` included, remains a path to cluster-admin: a Pod or Deployment runs
  as any ServiceAccount of its namespace, a `kubernetes.io/service-account-token` Secret receives
  a token for the ServiceAccount it names, and some clusters read access rules from ConfigMaps
  (EKS `aws-auth`).
- Contain Applications with AppProjects (allowed destinations, namespaces and kinds), or use
  `rbac.scope: namespaced` with `rbac.managedNamespaces` for a controller that can write only
  in listed namespaces.

## Config Management Plugins

Each entry of `repoServer.cmp.plugins` adds a sidecar to the repo-server pod. An init container
copies `patrolcd` from the Patrol image into a shared volume and the sidecar runs
`/var/run/patrol/patrolcd cmp-server` from the plugin's own image as uid 999. The plugin
configuration comes from an existing ConfigMap (`configMap`) or inline (`pluginConfig`).

The `tmpl-render` example (`examples/apps/cmp-plugin`):

```bash
kubectl -n patrol apply -k examples/apps/cmp-plugin/plugin     # ConfigMap patrol-cmp-tmpl-render
```

```yaml
repoServer:
  cmp:
    plugins:
      - name: tmpl-render
        image:
          repository: busybox          # needs sh, awk, sed, tr, sort, env
          tag: "1.37.0"
        configMap: patrol-cmp-tmpl-render
```

Plugins never receive repository credentials or a Kubernetes token, and only `PATH`, `HOME`,
`TMPDIR` and `LANG` of the sidecar environment reach plugin commands unless `PATROL_CMP_PASS_ENV`
is set in the plugin's `env` ([cmp.md](../architecture/components/cmp.md)).

## Source hydrator

`hydrator.enabled: true` sets `hydrator.enabled` for the server and controller and deploys the
commit-server (`ci/hydrator.yaml`). Configure the commit author in `configs.cm`
(`commit.author.name`, `commit.author.email`) and write credentials as Repository objects with
write access (see `examples/apps/source-hydrator`).

## Observability

- Metrics Services: `<prefix>-server-metrics` (8083), `<prefix>-controller-metrics` (8082),
  `<prefix>-repo-server` (8084), `<prefix>-applicationset-controller` (8080),
  `<prefix>-notifications-controller-metrics` (9001), `<prefix>-commit-server` (8087). Names
  are cut to 63 characters, before the `-metrics` suffix (a 31 to 40 character prefix shortens
  `notifications-controller`).
- `metrics.serviceMonitor.enabled` and `metrics.prometheusRule.enabled` (Prometheus Operator).
  Alerts: repo-server error ratio, invalid internal tokens, comparison failures, failed syncs,
  a controller shard without owner, notification delivery failures.
- Tracing: `tracing.otlpAddress` (OTLP/gRPC) for server, controller and repo-server; headers
  (authentication) only from `tracing.headersSecret`. Their NetworkPolicies then allow egress to
  the collector port; narrow it with `tracing.networkPolicy.to` (the collector's namespace and
  pods, or its CIDR) and set `tracing.networkPolicy.port` when the address has no port.
- Profiler: set `<component>.profile.enabled: "true"` in `configs.params` (mounted as a file,
  served on the metrics port).

## Air-gapped installation

`ci/airgap.yaml`: `global.imageRegistry` rewrites the registry of every image (Patrol, Dex,
plugins), `global.imagePullSecrets` authenticates, `image.digest` pins the mirrored image.
Mirror `patrol-cd/patrol-cd:<tag>` and, with Dex, `ghcr.io/dexidp/dex:v2.45.1` (the chart pins
its index digest). Set `configs.sshKnownHosts` to your Git hosts' keys (instead of the public
providers' defaults; Helm then owns them, see [Trust material](#trust-material)), add internal
CAs to `configs.tlsCerts`, and narrow the egress values to your internal networks.

## Upgrades and rollback

```bash
helm upgrade patrol deploy/helm/patrol-cd -n patrol -f values.yaml --wait
helm rollback patrol <revision> -n patrol --wait
```

- CRDs are templates, not Helm's `crds/` directory, so `helm upgrade` upgrades them (Helm 4
  uses server-side apply, which handles the large ApplicationSet CRD). They carry
  `helm.sh/resource-policy: keep`. A rollback re-applies the older CRD schema. Fields are
  additive within `v1alpha1`; validation rules are not, such as the one that keeps a Cluster's
  `spec.server` from changing, and a rollback drops them. Check the release notes before rolling
  back across versions.
- Runtime material (`server.secretkey`, the admin password, the internal token) is not part of
  the manifests and survives upgrades and rollbacks.
- Changing `configs.params` rolls the pods (checksum annotation); `configs.cm` and the other
  settings objects are followed live.
- Helm 4 upgrades with server-side apply: a field the chart renders that someone else changed
  (`kubectl edit`, `kubectl scale`, an API write to a values-managed key) fails the upgrade with
  `UPGRADE FAILED: conflict occurred while applying object …` and leaves the release `failed`
  with the objects before the conflicting one already applied. Either move the change into your
  values, or re-run with `--force-conflicts` to revert it. Fields the product changes at runtime
  are not rendered by default: the API-managed trust entries ([Trust material](#trust-material)),
  the keys the bootstrap and patrol-server add to `patrol-secret`, and the replica counts of
  autoscaled Deployments.
- Two installations must not both install CRDs or the admission policy: set `crds.install:
  false` and `crds.admissionPolicy: false` on all but one.

## Uninstall

```bash
helm uninstall patrol -n patrol
```

Kept on purpose: the CRDs and the admission policy (`crds.keep`; deleting a CRD deletes every
Application, project, cluster and repository of that kind) and two of the Secrets created at
runtime, `patrol-internal-token` and `patrol-initial-admin-secret`. `patrol-secret` is rendered
by the chart and is removed with the release, including `server.secretkey` (every session and
API token becomes invalid) and the admin password hash.

The trust ConfigMaps (`patrol-ssh-known-hosts-cm`, `patrol-tls-certs-cm`,
`patrol-gpg-keys-cm`) are release objects too: `helm uninstall` deletes them with every entry
the bootstrap seeded or the API added (`patrol cert`, `patrol gpg`, the UI). Repositories and
their credential Secrets created through the API are not release objects and survive, so after
a re-install SSH and TLS repositories on hosts outside the seeded defaults (and GnuPG
verification) fail until their entries are added again. Export the entries
before uninstalling and re-add them after the re-install (with `patrol cert add-ssh`/`add-tls`
and `patrol gpg add`, or `configs.sshKnownHosts`/`tlsCerts`/`gpgKeys`):

```bash
kubectl -n patrol get configmap patrol-ssh-known-hosts-cm patrol-tls-certs-cm patrol-gpg-keys-cm \
  -o yaml > patrol-trust-backup.yaml
```

A re-install finds the kept `patrol-initial-admin-secret` older than the new `patrol-secret`;
the bootstrap deletes it (its Role may delete that one Secret, by name) and generates a new
initial password instead of hashing the old one again, which would revert a changed admin
password (finding DV-6 in `docs/verification/phase4-findings.md`). Still delete the Secret as
soon as the initial password has been changed, and remove both kept Secrets after a final
uninstall:

```bash
kubectl -n patrol delete secret patrol-internal-token patrol-initial-admin-secret
kubectl delete validatingadmissionpolicybinding,validatingadmissionpolicy operations.cd.patrol.example
kubectl delete crd applications.cd.patrol.example applicationsets.cd.patrol.example \
  appprojects.cd.patrol.example clusters.cd.patrol.example operations.cd.patrol.example \
  repositories.cd.patrol.example repositorycredentials.cd.patrol.example scmproviders.cd.patrol.example
```

Remove Applications with cascading deletion first if their resources must go too.

## Without Helm

Install the CRDs and the admission policy with server-side apply (client-side apply fails on the
ApplicationSet CRD's size):

```bash
kubectl apply --server-side --force-conflicts -f deploy/crds
```

Then render the chart (no hooks, no `lookup`, deterministic output) and apply it with
server-side apply:

```bash
helm template patrol deploy/helm/patrol-cd -n patrol --kube-version 1.36.0 -f values.yaml > patrol.yaml
kubectl create namespace patrol
kubectl apply --server-side -n patrol -f patrol.yaml
```

Pre-rendered install manifests of the common variants are in `deploy/manifests`
([manifests.md](manifests.md)). Reapply the same way to upgrade; the trust ConfigMaps behave as
under [Trust material](#trust-material) (API changes survive a re-apply unless your values set
the same keys).

## Known limitations

- Installed from this repository only on local kind clusters (default profile, kindnet,
  linux/arm64, one replica per component; `docs/verification/runs/2026-10-19-kind-validation.md`).
  There the probes passed and NetworkPolicy enforcement was checked for the repo-server's egress
  (allowed and refused connections) and for metrics scraped from another namespace; the other
  components' policies, ingress refusals, other CNIs, HA and the other profiles are unverified.
- Dex: no configuration launcher (OPS-152); the `patrol` client secret is a manual step; memory
  storage, one replica.
- SSO: a key set with an RSA key above 4096 bits or a malformed Ed25519 key is refused as a whole
  ([Single sign-on](#single-sign-on)); Argo CD v3.5.5 accepts it. Tested against a fake IdP only,
  so whether a real IdP publishes such a key is not known.
- The controller tree API does not support client certificates (no mTLS for that hop).
- `selfSigned` internal TLS does not verify certificates (Argo CD's default behavior).
- `/download/patrol-*` CLI downloads are not populated by the image (see images.md).
- One installation per namespace (fixed settings object names).
