# Component: CLI core mode (`patrol --core`, `internal/cli/core`)

Status: implemented (API-055); the Kubernetes port-forward to the repo-server and the
controller is verified against a fake Kubernetes API only (API-054 in progress for this
use, see [Verification](#verification)). Upstream baseline: Argo CD v3.5.5
`cmd/argocd/commands/headless/headless.go`, `docs/operator-manual/core.md` (behavior
re-implemented, no code copied). Command reference:
[`docs/reference/cli-core-mode.md`](../../reference/cli-core-mode.md). Parity overlay:
`docs/parity/status/cli-core-mode.yaml`.

| Path | Contents |
|------|----------|
| `internal/cli/core/core.go` | `Provider` (`transport.Provider`): kubeconfig, namespace, in-process server, connection and its `Close` |
| `internal/cli/core/memtransport.go` | `HandlerTransport`: `http.RoundTripper` serving requests with an `http.Handler` in the process (streams, WebSocket upgrades through `Hijack`) |
| `internal/cli/core/pods.go` | Repo-server pod selection and controller pod verification, dialing through `transport.PodDialer` |
| `internal/cli/core/kubecontexts.go` | Namespace resolution; destination cluster configurations from the caller's kubeconfig contexts |
| `internal/cli/core/endpoints.go` | `Endpoints` override (tests, debugging) and its validation |
| `internal/cli/core/register.go` | `Register` / `Registrar`: installs the provider on the CLI factory and refuses session commands |
| `internal/cli/transport/podstream.go` | `PodDialer`: pods/portforward WebSocket streams as in-memory `net.Conn`s (no listener) |
| `internal/server/core.go`, `core_support.go` | `NewCoreHandler(ctx, CoreConfig)`: the patrol-server API assembled for core mode |
| `internal/server/core_clusters.go` | `coreClusterConnector` (cluster connection checks and rotate-auth with the caller's credentials), `coreNamespaceScoped` (ApplicationSet lists and watches scoped to the Patrol namespace) |
| `internal/cli/cmdutil` | `PATROL_CORE`, conflicting flag checks (`coreConflicts`), the kubectl client flags (`AddKubectlFlags`) |
| `cmd/patrol/main.go` | `core.Register` is the last registrar |

## Behavior

`patrol --core <command>` (also `PATROL_CORE=true`, `--core` in `PATROL_OPTS`, or a context
stored by `patrol login --core`, whose server is `kubernetes` with `core: true`) runs the
patrol-server API inside the CLI process for the duration of the command. No Patrol CD server
is contacted.

```
patrol command ─► pkg/apiclient ─► http.Client{Transport: HandlerTransport}   (no socket)
                                        │
                                        ▼
                   server.NewCoreHandler: apiserver + services (DisableAuth)
                    │  all Kubernetes calls: caller's kubeconfig credentials
                    │  repo-server / controller tree API: Dial ─► podRouter
                    ▼                                               │
            Kubernetes API server ◄── pods/portforward (WebSocket) ◄┘  PodDialer
```

1. The kubeconfig is loaded from the command's environment (`--kubeconfig`, `KUBECONFIG`,
   `$HOME/.kube/config`) with the kubectl override flags (`--context`, `-n/--namespace`,
   `--user`, ...) and `--kube-context`. The kubectl flags are persistent flags of the command
   groups (`app`, `appset`, `cluster`, `repo`, `repocreds`, `proj`, `account`, `cert`, `gpg`,
   `configure`, `version`), so every subcommand accepts them, before or after the group name
   (`patrol --core app list -n patrol-staging`, `patrol --core -n patrol-staging app get web`;
   `TestKubectlFlagsOnSubcommands`, `TestCoreNamespaceFlag`). A subcommand's own flag of the
   same name takes precedence on that subcommand: `cluster add/set --namespace` (the
   cluster's namespaces), `cluster add/rm --kubeconfig` (the cluster's kubeconfig), `repo
   add` / `repocreds add` `--username`, `--password`, `--proxy-url`. Client tuning: QPS 50,
   burst 100 unless the kubeconfig sets them; User-Agent `patrol-cli/<version> (core mode)`.
2. Namespace of Patrol (settings, projects, clusters, repositories, Applications):
   `-n/--namespace`, else the namespace of the selected kube context, else `patrol` (upstream
   falls back to `default`). `--port-forward-namespace` is not used (it conflicts with
   `--core`).
3. `server.NewCoreHandler` reads, once and with the caller's credentials: the caller's
   identity (`SelfSubjectReview`, `authentication.k8s.io/v1`), the settings objects
   (`patrol-cm`, `patrol-rbac-cm`, `patrol-cmd-params-cm`, TLS/SSH/GPG/notifications
   ConfigMaps, `patrol-secret`, `patrol-notifications-secret`, Secrets labelled
   `app.kubernetes.io/part-of=patrol-cd`) and the server parameters of
   `patrol-cmd-params-cm`. Missing or forbidden objects are treated as absent; an unreadable
   `patrol-cm` prints a warning (`core mode: ConfigMap <ns>/patrol-cm is not readable ...`)
   because diffs and manifests may then differ from the controller's. Any other error (API
   server unreachable, credentials rejected) fails the command. Nothing is written at
   startup: no signing key bootstrap, no default project creation, no token normalization.
4. The API is the patrol-server route table (same services as `server.Run`, assembled with
   the exported library constructors) minus proxy extensions (disabled, as upstream) and SCM
   webhook receivers; `TestCoreRoutesMatchTheServer` fails when a documented server route
   is missing. Authentication and Patrol RBAC are disabled (`DisableAuth`):
   `patrol-rbac-cm` and project roles are not evaluated. Reads are uncached (no informer
   cache at startup); the Application watch stream starts an Application informer on the
   first subscription, the repository credentials resolver starts its informers on first
   use (bounded by 30 s), both only after `SelfSubjectAccessReview`s show the caller may list
   and watch the objects.
5. The CLI's API client gets an `http.Client` whose transport is `HandlerTransport`: the
   request is served by calling the handler; the response body is an `io.Pipe` (streams
   arrive as written), `SetWriteDeadline` is honored, upgrades (`Connection: Upgrade`) are
   served through `http.Hijacker` over `net.Pipe`. No listener is opened, no port bound; the
   server's metrics and health listeners are not started. The connection reports
   `AuthDisabled`, so the CLI never sends a Patrol token, and nothing is written to the CLI
   configuration (only `login --core` stores the token-less core context).
6. Closing the connection (end of command) cancels requests in flight, ends response bodies
   and hijacked connections, waits for handlers (bounded, 10 s), stops lazily started
   informers, closes idle connections and every port-forward stream.

## Identity and authorization

| Aspect | Core mode |
|--------|-----------|
| Kubernetes calls of the in-process server | Always the caller's kubeconfig credentials: `CoreConfig.RestConfig` for the control plane, the caller's kubeconfig context for the server of a destination cluster (`CoreConfig.DestinationConfig`, see [Destination clusters](#destination-clusters)). Never a service account: not the control plane's identity, not the credentials stored in Cluster Secrets (patrol's manager service account on remote clusters), not credentials sent in a request (`TestCoreClusterConnectionsUseCallerCredentials`, `TestCoreClusterCommandsUseCallerCredentials`). The kubeconfig's own impersonation (`as:`) applies as configured. |
| Authorization | Kubernetes RBAC of the caller only. A forbidden call fails the command with the Kubernetes message appended to the error, e.g. `permission denied: appprojects.cd.patrol.example is forbidden: User "alice" cannot list resource "appprojects" ...` (in server mode these messages describe the server's identity and are hidden). |
| Principal of API requests | The `SelfSubjectReview` user: a per-process token naming it is added to each request by the handler wrapper and verified as an external token (`coreVerifier`, constant-time comparison); a token sent by the client is discarded. Audit log lines, audit Events and `account get-user-info` show this user. Without an answer from `SelfSubjectReview` requests are anonymous. |
| `Operation.spec.initiatedBy` | `username` = the `SelfSubjectReview` user name, `automated` = false. The Operation admission policy (`deploy/crds/operations-admission-policy.yaml`) admits a username that is empty or equal to `request.userInfo.username` without the `impersonate` verb, and the Operation is created with the caller's own credentials, so the request is always admitted in this shape; the caller never needs (and core mode never uses) the `impersonate` verb on `operations`. Without a `SelfSubjectReview` answer `username` is empty, which the policy also admits. Clients cannot choose the value (the sync request has no such field). |
| Audit Events | Recorded synchronously with the caller's credentials (source component `patrol-cli`); a caller without `create events` gets none (best effort). The Kubernetes audit log records the caller for every request. |
| Project tokens (`proj role create-token`) | Issued only with the real signing key (`patrol-secret` `server.secretkey` readable by the caller); otherwise `project tokens cannot be issued in core mode without the session signing key ...`. |
| Session routes | Refused by the handler with 412 and `<operation> is not available in core mode: there are no Patrol CD sessions; the Kubernetes API authenticates and authorizes every request with your kubeconfig credentials`: `POST/DELETE /api/v1/session`, `POST /api/v1/session/oidc-exchange`, `/auth/login`, `/auth/callback`, `/api/dex/`, `PUT /api/v1/account/password`, `POST/DELETE /api/v1/account/{name}/token[/{id}]`. |

Kubernetes permissions per use (namespace of Patrol unless noted):

| Use | Permissions |
|-----|-------------|
| Every command | `selfsubjectreviews` create (granted to all authenticated users by default); optional: `configmaps` get (settings), `secrets` get/list (settings, referenced Secrets) |
| Applications | `applications` get/list (`app list`), plus `appprojects` get/list and `repositories`, `clusters` list for reads that check the project (`app get`); create/update/patch/delete for changes; `operations` create/get/list for sync, rollback, terminate-op; `applications` watch for `app wait` and sync progress |
| ApplicationSets | `applicationsets` get/list/watch in the Patrol namespace (lists and watches are scoped to it: core mode enables no other ApplicationSet namespace); create/update/delete for changes |
| Clusters | `clusters` get/list, create/update/delete for changes, `secrets` get/create/update on the credentials Secret; connection checks and `rotate-auth` use the caller's own permissions on the destination cluster (see below) |
| Rendering, diff, repository checks | `repositories`, `repositorycredentials`, `scmproviders`, `secrets` list/watch; `secrets` get on `patrol-internal-token`; `pods` list/get and `pods/portforward` get/create (repo-server pods) |
| Resource tree, managed resources, server-side diff | `secrets` get on `patrol-internal-token`; `clusters` get/list, `leases` get (shard routing); `pods` get and `pods/portforward` get/create (controller pods) |
| Logs, events, live resources, actions | The caller's own permissions on the destination cluster (see below) |

Whoever may read `patrol-internal-token` can call the repo-server and the controller tree
API directly (rendering with credentials resolved from the Secrets they could read anyway,
live manifests of managed resources with Secrets masked); grant it only to operators. Users
without it can still use every command that only reads and writes Patrol objects.

## Repo-server and controller

- Addresses: the repo-server address comes from `repo.server` of `patrol-cmd-params-cm`
  (default `patrol-repo-server:8081`, HTTPS unless `server.repo.server.plaintext`); the tree
  API uses the routing of patrol-server: the shard of the Application's destination cluster
  (`Cluster.status.assignedShard`, else `spec.shard`, else 0; `project.ResolveDestination`
  over the Cluster objects), the holder of Lease `patrol-controller-shard-<n>`, else the
  ordinal pod `patrol-controller-<n>`, through `treeapi.ShardRouter` (plain HTTP on port
  8090, the server's default). The internal transport maps exactly these addresses to
  `CoreConfig.Dial` targets (`repo-server`, or `controller` + pod + port); any other address is
  refused (`core mode: no route to ...`).
- `podRouter` (CLI): the repo-server pod is a Running, Ready, not deleting pod labelled
  `app.kubernetes.io/name=<--repo-server-name>` (default `patrol-repo-server`), first by name;
  it is cached and a failed dial selects again (pod restarted, next ready pod). A controller
  pod named by shard routing must exist in the namespace, be Running and Ready, and carry
  `app.kubernetes.io/name=<--controller-name>` or be owned by StatefulSet
  `patrol-controller`; otherwise the internal token is not sent (`pod ... selected by shard
  routing is not a controller pod ...`). Verification is cached for 30 s and dropped when a
  dial fails.
- `PodDialer` opens one `pods/<pod>/portforward?ports=<port>` WebSocket stream
  (`v4.channel.k8s.io`, the `--port-forward` machinery: same handshake checks, frame
  limits and error channel handling) per connection and returns one end of a `net.Pipe`,
  so no local port exists. The HTTP transport pools these connections (keep-alive) and
  reconnects on demand. TLS to the repo-server is not verified (patrol-server's default;
  the internal token authenticates and the Kubernetes API server selects the pod over the
  kubeconfig's verified connection).
- Internal token: read on first use from Secret `patrol-internal-token` (key `token`) with
  the caller's credentials and cached for the command; failures are appended to the
  command's error.

### Endpoints override (tests and debugging)

`core.Options.Endpoints` (Go) or `PATROL_CORE_ENDPOINTS=repo-server=<url>,controller=<url>`
honored only with `PATROL_CORE_ALLOW_ENDPOINTS=true` (otherwise the command fails) replaces
port-forwarding with fixed URLs. URLs must be `http(s)://host[:port]` with a loopback host
(`localhost`, `127.0.0.0/8`, `::1`) or a cluster-internal name (`*.svc`,
`*.svc.cluster.local`): the internal token cannot be redirected to an arbitrary host. The
token is read from the cluster Secret with the caller's credentials; only the Go option may
carry an explicit token (tests). This is not a product setting.

## Destination clusters

Every call to a destination cluster uses the caller's kubeconfig: the in-cluster address
(`https://kubernetes.default.svc`) and the selected context's cluster use the selected
context; another cluster uses the first kubeconfig context (by name) whose cluster has the
same server URL. Only the server URL selects the context; the connection settings of the
Cluster (TLS, proxy, exec plugin, AWS) are not applied to the caller's credentials. The
credentials stored in Cluster Secrets for patrol-server (on a remote cluster, patrol's
manager service account) and credentials sent in a request are never used, so the
destination cluster authenticates, authorizes and audits the caller.

| Call | With a kubeconfig context for the server | Without one |
|------|------------------------------------------|-------------|
| Resource routes (pod logs, events, live resources, actions, web terminal) | Caller's context | `... the configuration of cluster <url> is not available: core mode: no kubeconfig context for destination cluster <url>; core mode reads live resources with your own kubeconfig credentials` (HTTP 503). The provider's error implements `UserMessage()`, which the applications service appends to the response (`clusterConfigUnavailable`); the cause of any other provider error stays out of the response. |
| Preview of a cluster whose `Cluster.status` lacks its version or API versions (`GET .../manifests`, manifests-with-files) | Caller's context answers (`/version`, discovery) | Not asked: nothing is sent to the cluster and the render has what the status holds, so `app manifests --revision`, `app diff --revision` and `app sync --label` work as with a status that has both |
| Connection check of `cluster add`, `cluster set`, upsert (`GET /version`) | Caller's context; the result is recorded in `Cluster.status` (connection state, server version), which the controller replaces with its own check | Not checked: nothing is sent to the cluster, the change is saved and no connection state is recorded (the controller checks the connection with the stored credentials, as for exec/AWS clusters in server mode) |
| `cluster rotate-auth` | Caller's context: reads the token Secret named by the stored token's claims, creates the new token Secret, updates the service account and deletes the old Secret with the caller's permissions there (`secrets` get/create/delete, `serviceaccounts` get/update in that namespace); the new token is stored without a separate check (the connection check uses the caller's context) | Refused before anything changes: `core mode changes a cluster only with your own kubeconfig credentials for it, never with the credentials stored for Patrol CD: ...` |

Upstream core mode authenticates these checks and the rotation with the stored cluster
credentials, as patrol-server does.

## Commands

| Command | Core mode |
|---------|-----------|
| `login --core` | Stores context `kubernetes` (`core: true`, no token) without contacting anything (upstream) |
| `logout <core context>` | Removes nothing but succeeds (no token; upstream) |
| `relogin` (current context is core) | `relogin is not available in core mode: ...` (exit 20). Upstream fails parsing the missing token. |
| `account update-password`, `generate-token`, `delete-token`, `session-token` | `<command> is not available in core mode: ...` (exit 20); also refused by the handler |
| `account get-user-info` | The Kubernetes user (`Logged In: true`) |
| `admin dashboard --core` | `admin dashboard is not available in core mode: Patrol CD never serves the web UI from an unauthenticated in-process server ...` (difference by design; without `--core` the dashboard tunnels to patrol-server) |
| `version` | Client version and the in-process server version with `mode: core (in-process API server in the CLI; no Patrol CD server contacted)`; no tool versions (rendering runs in the repo-server) |
| `app watch`-style streams, `app wait`, `app sync` progress, `app logs`, web terminal | Through the in-process handler (SSE streams, WebSocket upgrades) |
| Everything else | As against patrol-server, authorized by Kubernetes RBAC |

Conflicts (exit 1, before anything is contacted): `--core` with `--server` / `PATROL_SERVER`,
`--core` with `--port-forward` / `--port-forward-namespace`, core mode (flag or context)
with `--auth-token` / `PATROL_AUTH_TOKEN`; invalid `PATROL_CORE` values.

## Differences by design

| Upstream | Patrol | Reason |
|----------|--------|--------|
| In-process server listens on `localhost:<random>` (plain HTTP, auth disabled) | No listener; in-memory transport | Any local process could use the caller's Kubernetes identity through an unauthenticated port |
| Namespace fallback `default` | `patrol` | Default install namespace |
| Server logs at error level | Server logs only with `--loglevel debug`; failures surface as command errors | CLI output |
| Redis cache via port-forward (`--redis-*`) | No Redis (ADR-0004); resource tree from the controller tree API | Architecture |
| `ARGOCD_FAKE_IN_CLUSTER` for the in-cluster destination; remote destinations, cluster connection checks and `rotate-auth` with cluster Secret credentials | In-cluster and remote destinations, connection checks and `rotate-auth` with the caller's kubeconfig contexts only; without one, checks are skipped and `rotate-auth` is refused | Caller's credentials only |
| kubectl flags only on the command group itself (`argocd app list -n x` is rejected) | Persistent: accepted by every subcommand | `-n` as documented for core mode |
| `admin dashboard` serves the UI from an unauthenticated in-process server | Refused with `--core` | As above |
| Kubernetes forbidden messages hidden | Appended to errors | They describe the caller's own access |
| No `PATROL_CORE`-like variable (only `ARGOCD_OPTS`) | `PATROL_CORE` | Convenience; `PATROL_OPTS` wins |
| `--core` with `--server` / `--auth-token` silently ignored | Refused | Clarity, no token sent anywhere |
| Startup creates the default project | Nothing written at startup | Commands never write implicitly |

## Gaps

- Run by hand on a kind cluster (2026-10-19, `docs/verification/runs/2026-10-19-kind-validation.md`,
  step 15; no committed script or test repeats it): `app list`, `app get`, `app manifests` (the repo-server through a kubelet-backed
  port-forward and the controller of a real StatefulSet), `app diff` and `app history` work in
  core mode against the live control cluster. Not exercised against a kubelet: `app logs`, live
  resources, actions and the web terminal of a destination cluster (they need a kubeconfig
  context whose server URL equals the Cluster's `spec.server`; the kind Cluster uses a
  container-network name the host cannot reach), `cluster add/rotate-auth`, and `appset`.
- The tree API is reached in plain HTTP; controllers serving it with `--tls-cert` are not
  reachable in core mode (the server's `--controller-tree-api-tls` is a flag, not a
  parameter core mode can read).
- `server.repo.server.strict.tls` and its CA path are not honored (pod file paths).
- Settings are read once per command (no change following during long `app wait`).
- Watch streams started by a caller who may list but not watch Applications send the
  initial list only.

## Verification

See `docs/verification/log.md` (V-CLICOREMODE-01) and
`docs/verification/runs/2026-10-16-cli-core-mode.txt`. Tests:

- `internal/cli/core` (envtest kube-apiserver 1.36 with the CRDs, the Operation admission
  policy and a Metadata audit log; repo-server served by the real repo-server handler over a
  fake service, the real tree API handler over a stand-in state, both on loopback through the
  Endpoints override; a controller stand-in executes Operations):
  `TestCoreAppLifecycle` (version, user info, proj create/list/get/delete, repo add/list,
  app create/list/get/diff/sync/wait/diff/delete with a namespace-scoped RBAC user without
  `impersonate`, `initiatedBy` = the Kubernetes user, no stored token, every audited request
  of the in-process server made by that user without impersonation);
  `TestCoreKubernetesRBACIsTheAuthorization` (`patrol-rbac-cm` grants nothing; a user with
  only get/list on Applications lists them, gets Kubernetes forbidden errors naming the user
  for get (projects), create, delete and `proj list`; a read-only user's create is denied by
  Kubernetes on `applications`; audit log: only those users, no impersonation);
  `TestCoreConnectionInProcessAndCleanShutdown` (HandlerTransport, `kubernetes` address,
  auth disabled, lazy informers and an open event stream, goroutine count back to the
  baseline after `Close`; a mutation run without stopping the server failed it, 29 → 59);
  `TestCoreNamespaceFlag` (`-n`, `--namespace`, `--context`, `--kubeconfig` on subcommands,
  before or after the group name, over a context whose namespace is another one; `appset
  list` with a namespace-scoped user); `TestCoreClusterCommandsUseCallerCredentials`
  (`cluster set` / `rotate-auth` against a TLS stand-in destination: without a context for
  it nothing is sent and rotate-auth is refused; with one only the caller's token is sent,
  never the stored service-account token);
  `TestConflictingFlags`, `TestUnsupportedCommands`, `TestRegistrarKeepsAnExistingProvider`,
  `TestKubectlFlagsOnSubcommands` (every command merges its flags; `-n` is the kubectl
  namespace on every subcommand that has it; `cluster add/set` keep their `--namespace`);
  `TestHandlerTransport*` (requests, streams, cancellation, write deadline, WebSocket,
  panics); `TestPodRouter*` (fake clientset); `TestPortForwardRouting` (real `PodDialer`
  against a fake Kubernetes API speaking `v4.channel.k8s.io`, bridging to loopback test
  servers; 403 handling; close); endpoint validation and gating; namespace and destination
  resolution.
- `internal/server`: `TestCoreHandlerEnvtest` (shard routing to the Lease holder and the
  ordinal pod, repo-server with the token read from the Secret, session routes refused,
  principal from `SelfSubjectReview`, client tokens ignored, project tokens refused without
  the signing key), `TestCoreHandlerDegrades` (anonymous without `SelfSubjectReview`, sync
  then creates its Operation with an empty initiator; unreadable internal token reported),
  `TestCoreClusterConnectionsUseCallerCredentials` (cluster update and rotate-auth through
  the handler against a TLS stand-in destination whose stored credentials would connect:
  without `DestinationConfig` for it no request reaches it; with one every request carries
  the caller's token; with the default connector the stand-in received the stored token,
  which fails the test), `TestCoreRoutesMatchTheServer`, `TestCoreDialTargets`,
  `TestCoreConfigValidate`, `TestDiagnosticWriter`.
- `internal/cli/transport`: `TestPodDialerArgumentsAndClose`.
