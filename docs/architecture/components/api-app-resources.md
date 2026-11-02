# Component: resource-level application API (`internal/apiserver/services/applications`)

Status: implemented (resource tree and its stream, managed resources, server-side diff, events,
live resource get/patch/delete, resource actions, pod logs, web terminal; not yet wired into the
`patrol-server` command). Normative contract: [`api-contract.md`](../api-contract.md#applications).
Framework: [`apiserver.md`](apiserver.md). Core application routes:
[`api-applications.md`](api-applications.md) (same `Service`, same existence-hiding rules).
Collaborators: [`treeapi.md`](treeapi.md) (controller state), [`clusters.md`](clusters.md)
(destination credentials), [`project.md`](project.md) (destinations, resource permissions,
service-account selection), [`lua-health.md`](lua-health.md) (actions), [`diff.md`](diff.md)
(masking). Upstream baseline: Argo CD v3.5.5 `server/application/application.go`
(`ResourceTree`, `WatchResourceTree`, `ManagedResources`, `ServerSideDiff`, `ListResourceEvents`,
`GetResource`, `PatchResource`, `DeleteResource`, `ListResourceActions`, `RunResourceActionV2`,
`PodLogs`, `getApplicationClusterConfig`), `logs.go`, `terminal.go`, `websocket.go` (behavior
re-implemented, no code copied).

| File | Contents |
|------|----------|
| `resource_deps.go` | `ResourceDeps` (collaborators and limits), `TreeClient`, `ClusterConfigs`/`ClusterConfigsFunc`, `ActionRuntime`/`LuaRuntimes` (default: `lua.New(settings.ResourceOverrides())` per settings fingerprint), `LogsConfig`, `TerminalConfig`, the `LiveResources` adapter used by resource links |
| `resource_routes.go` | Route registration (appended to `routeRegistrars` in `routes.go`) |
| `tree.go` | Tree, tree stream, managed resources, controller error mapping, cache-miss refresh, Secret re-masking |
| `diff.go` | Server-side diff |
| `resource.go` | Destination cluster configuration (impersonation), destination error mapping, discovery, events, live get/patch/delete, fine-grained RBAC |
| `actions.go` | Action discovery and execution (patch/create, status subresource, project checks, dry runs, Application self-actions) |
| `logs.go` | Pod logs: query parsing, pod selection, per-pod readers, timestamp merge, SSE and download writers |
| `terminal.go`, `terminal_exec.go` | Web terminal (WebSocket bridge, checks, audit) and the Kubernetes exec client (WebSocket channel protocol v5/v4) |
| `pkg/apiclient/application_resources.go` | Route constants, request/response types, client methods (`GetApplicationResourceTree`, `WatchApplicationResourceTree`, `GetApplicationManagedResources`, `ApplicationServerSideDiff`, `ListApplicationEvents`, `Get/Patch/DeleteApplicationResource`, `List/RunApplicationResourceAction`, `StreamApplicationLogs`, `StreamPodLogs`, `ReadLogs`, `DownloadApplicationLogs`, `OpenTerminal` → `TerminalSession`) |

## Routes and RBAC

Object `<project>/<app>` (`<project>/<ns>/<app>` outside the control-plane namespace). Every
route first reads the Application with the existence-hiding rules of `getApp` (403 for missing
or forbidden without `?project=`, 404 with it); RBAC is checked before any controller or
cluster call.

| Method and path | RBAC | Behavior |
|------|------|----------|
| `GET /api/v1/applications/{name}/resource-tree` | applications, get | Controller tree (nodes, orphanedNodes, hosts), unfiltered as upstream (nodes carry no object data) |
| `GET /api/v1/stream/applications/{name}/resource-tree` | applications, get per event | SSE `{type, object: ApplicationTree}`: ADDED, then MODIFIED when the tree's ETag changes (poll every 3 s and on Application events, at most every 500 ms); the Application is re-read and re-authorized on every event; access lost → 403 error event; Application deleted/replaced → DELETED and end; controller outages are survived |
| `GET /api/v1/applications/{name}/managed-resources` | applications, get | Controller managed resources, hooks excluded, exact `group/kind/namespace/resourceName` filters (`version` ignored) |
| `POST /api/v1/applications/{name}/server-side-diff` | applications, get | Body `{liveResources, targetManifests}` (index-aligned, ≤ 5000, body ≤ 32 MiB). The API checks each live state against its declared identity (`name/namespace/group/kind mismatch: …` 400), then requires a target paired with a live state to name the same object (`target manifest <i> (<g>/<k> <ns>/<n>) does not match live resource <g>/<k> <ns>/<n>` 400), before calling the controller; the controller repeats the identity check, checks membership (`<g>/<k> <n> not found as part of application <app>` 403) and runs the dry runs with its own credentials |
| `GET /api/v1/applications/{name}/events` | applications, get | No resource: Events of the Application on the control plane (involvedObject name/namespace/UID). With `resourceName`/`resourceUID`/`resourceNamespace`: the node (managed or orphaned) must match all three, else 400 `<name> not found as part of application <app>`; Events read from the destination cluster with the Application's configuration. ≤ 1000 items |
| `GET /api/v1/applications/{name}/resource` | applications, get | Node with UID of the tree (managed or orphaned), else 400 `<kind> <group> <name> not found as part of application <app>`; `version` overrides the node's; live GET through discovery on the destination cluster; core Secrets masked |
| `POST /api/v1/applications/{name}/resource` | applications, `update/<group>/<kind>/<ns>/<name>` | Body `{patch, patchType: json|merge (default)|strategic}` (media types accepted); patch must be JSON; result masked; Secret failures report only `failed to patch Secret <ns>/<name>` (keeping the status); Event `<user> patched resource <group>/<kind> '<name>'` |
| `DELETE /api/v1/applications/{name}/resource` | applications, `delete/<group>/<kind>/<ns>/<name>` | `orphan` → Orphan; `force` → Background, grace 0; default Foreground; Event `deleted resource <group>/<kind> '<name>'` |
| `GET /api/v1/applications/{name}/resource/actions` | applications, get | Lua discovery (configured scripts for the exact `group/Kind`, then built-ins unless replaced); group `cd.patrol.example`, kind `Application`, `resourceName` = app → the Application itself |
| `POST /api/v1/applications/{name}/resource/actions` | applications, `action/<group>/<kind>/<action>` | Body `{action, resourceActionParameters}` (≤ 32 parameters, ≤ 64 KiB each); see "Actions" |
| `GET /api/v1/applications/{name}/logs`, `GET …/pods/{pod}/logs` | applications, get **and** logs, get | See "Logs" |
| `GET /terminal` (WebSocket) | applications, get **and** exec, create | See "Terminal" |
| `GET /api/v1/applications/{name}/resource/links` | applications, get | Core route; managed resources are read through the `LiveResources` adapter of this component (tree membership + live GET with the Application's configuration) |

Fine-grained RBAC (TEN-070/071): with `server.rbac.disableApplicationFineGrainedRBACInheritance`
true (default) only the fine-grained action counts; with `false` the plain verb on the
Application is tried first (silently) and suffices, otherwise the fine-grained one is enforced
(audited denial). The Application's project is used in both paths.

### Controller state (tree API)

Trees, managed resources and server-side diffs come from the controller replica serving the
Application's destination cluster (`treeapi.Client` with a `ShardRouter`: shard, Lease holder,
one re-route on 421, 200 MiB response cap). A missing state (`not_found`/`unavailable` answers,
upstream cache miss) reports the Application's `ComparisonError`/`InvalidSpecError` conditions
(412) or requests a normal refresh, waits for it (≤ 15 s, the core refresh) and retries once.
Controller validation messages (400/403) are returned; transport failures, wrong tokens and
internal errors are 503/500 with generic details. Managed-resource and server-side-diff items
are masked again at the API (core Secrets decided from the parsed objects, sensitive
annotations, pairs keep equal masks equal; unparsable states fail closed).

### Destination cluster configuration (SYNC-113)

`ClusterConfigs.RESTConfig` gives the cluster's own credentials (production: the
`internal/clusters` registry). Per request, `application.sync.impersonation.enabled` (exactly
`"true"`) selects impersonation: the first `destinationServiceAccounts` entry of the effective
project whose server glob matches the cluster URL and namespace glob the destination namespace
(`internal/project.SelectServiceAccount`); only `Impersonate-User` is set. No match →
412 `error getting application cluster config: no matching service account found for
destination server <s> and namespace <ns>` when enforced (default), else the cluster
credentials (info log). Impersonated: resource events, live get/patch/delete, action discovery
and execution (including the Application self path's resolution), pod logs, resource links.
Not impersonated: Application events, trees/managed resources/server-side diff (controller),
the web terminal (SYNC-123), a preview's question about the destination's versions, which the
controller's cache asks with the cluster credentials (`api-applications.md`). Destination Kubernetes errors keep their status and message with
the upstream prefix (`error getting resource: …`, `error patching resource: …`, `error deleting
resource: …`, `error listing resource events: …`); 401s and transport errors are 503 with
generic details.

### Actions

1. Authorize `action/<group>/<kind>/<action>`, load the object (live object of the tree, or the
   Application from the control plane).
2. Discovery runs; a disabled action → 412; parameters must be declared by the action's
   discovery entry (an undiscovered action run through the fallback lookup takes none) → 400.
3. The script runs (`internal/lua`, 1 s budget, safe library profile); unknown action → 400
   `error getting Lua resource action: …`, script errors 400 `error executing Lua resource
   action: …` (e.g. `invalid number: abc`).
4. ≤ 100 impacted resources; every one must pass `project.IsResourcePermitted` for the
   destination cluster (403 `application is not permitted to manage <g>/<k>/<n> in <ns>`);
   creations are dry-run (`dryRun=All`) before any write.
5. Writes in order: merge patch live → result (`{}` = no call), the status subresource first
   when status changes (NotFound → one full patch), creates. Not transactional (upstream).
6. The Application itself is patched in the control plane with the `resourceVersion` the action
   saw (409 on a concurrent change); an action may not change `spec.project` (403).
7. Event `ResourceActionRan` `<user> ran action <a>[ on resource <g>/<k>/<n>]`.

### Logs

Order: `untilTime`/`sinceTime`/numbers validated (400 `invalid untilTime parameter value: …`) →
applications, get (hiding) → logs, get (403 `permission denied: logs, get, …`; always enforced
as in Argo CD v3, the removed `server.rbac.log.enforce.enable` key has no effect) → tree →
cluster configuration → Pod nodes with UID of the managed tree matching `kind/group/
resourceName/namespace` directly or through parentRefs (cycle-safe; `podName` or the
`{pod}` path imply `kind=Pod`) → none → empty stream; more than `server.maxPodLogsToRender`
(default 10; `0` refuses every pod, as upstream, it does not mean unlimited) → 400 `max pods to
view logs are reached. Please provide more granular query`.
Each pod: `pods/<p>/log` with `timestamps=true`, `container`, `follow`, `sinceSeconds`,
`sinceTime`, `tailLines`, `previous`; a stream that cannot be opened becomes one line with the
Kubernetes message. Lines are split on `\r`, merged by timestamp (100 ms window, flush at
10 000 buffered lines), filtered (`filter` substring, leading `!` inverts, `matchCase`), and
sent as `LogEntry{content, timeStamp, timeStampStr, podName, last}`; the first line after
`untilTime` is sent with `last: true`, otherwise a final empty entry with `last: true`.
`download=true` returns `application/octet-stream`, `attachment;filename="<ns>-<pod>-<container>.log"`
(`log.log` unless all three are valid names), one content line per entry.

Bounds: 64 KiB per line (truncated with ` ...[truncated]`), 2000 lines/s (burst 10 000; the pod
readers are paused, not dropped), 256 MiB of content per stream (then `event: error` 429),
blocking writes with a 30 s deadline (a slow client slows the pod readers instead of growing
buffers), heartbeat comments every 15 s, and every minute the caller's session is re-verified
(revoked/expired → `event: error` 401) and the Application and `applications, get` + `logs, get`
re-checked (→ 403). Client disconnect cancels every pod stream. Log content is untrusted: it is
only JSON-encoded (SSE) or written as an attachment (download).

### Terminal

`GET /terminal?appName=&appNamespace=&projectName=&namespace=&pod=&container=&shell=` (outside
`/api/v1`, as upstream and the UI). Checks in order: `exec.enabled` (404), same-origin
handshake (an `Origin` must name the request host, `url` or an `additionalUrls` host; `null`
and other origins 403 — cookie-authenticated cross-site WebSocket hijacking is not possible),
required parameters (400 `Missing required parameters`), name validation (400 `Pod name is not
valid`, …), Application namespace enabled (403), applications, get (hiding; a wrong project is
404), exec, create (403), Pod node of the managed tree (400 `Pod doesn't belong to specified
app`), running (init/ephemeral) container (400). The exec uses the cluster's own credentials
(never impersonated). Shell: `shell` when listed in `exec.shells`, otherwise each configured
shell in order until one starts (a start failure, not an exit code, before any input).

Session: TerminalMessage JSON text frames (`stdin`, `resize` from the client; `stdout` from
the server); client messages ≤ 64 KiB; unknown operations or invalid frames end the session;
`applications, get` and `exec, create` re-checked on every message, and every 30 s the caller's
session (`rest.Reverify`), the Application (same UID and RBAC object) and permissions; a denial
sends `Permission denied` and closes with 1008. Idle timeout 30 min, pings every 15 s,
optional maximum duration. Audit entries (`openTerminal`, resource exec, action create, the
Application as target, Events `TerminalSessionStarted`/`TerminalSessionEnded`) record actor,
Application, pod, container, end reason and duration; never terminal content.

The exec client speaks the Kubernetes WebSocket channel protocol (`v5.channel.k8s.io`,
`v4.channel.k8s.io`; stdin 0, stdout 1, stderr 2, status 3, resize 4) with
`github.com/coder/websocket`, authenticated through `rest.TLSConfigFor` and
`rest.HTTPWrappersForConfig` (client certificates, bearer tokens and files, exec plugins,
proxies). `k8s.io/client-go/tools/remotecommand` (SPDY and its WebSocket executor) needs modules
that are not in `go.mod` (`github.com/gorilla/websocket`, `k8s.io/streaming`), so there is no
SPDY fallback; every supported Kubernetes version serves exec over WebSocket.

## Wiring (`Deps.Resources`)

| Field | Production value |
|-------|------------------|
| `Tree` | `treeapi.NewClient(&treeapi.ShardRouter{Shards: sharding.StatusShardResolver{…}, Holders: sharding.NewLeaseHolders(…), Namespace: ns, Service: <headless service>}, internalToken)` |
| `Clusters` | `applications.ClusterConfigsFunc(func(ctx, c) { return registry.RestConfig(ctx, c, clusters.Options{}) })` (`*clusters.Registry`) |
| `Lua` | nil (default per settings snapshot) |
| `Auditor` | the server's `rest.Auditor` (`LogAuditor` with the Event recorder), so terminal sessions appear in the audit log and as Events |
| limits | defaults (`TreePollInterval` 3 s, `ClusterCallTimeout` 1 min, `MaxEvents` 1000, `LogsConfig`, `TerminalConfig`) |

`Deps.LiveResources` may stay nil: `New` installs the tree-API adapter when `Tree` and
`Clusters` are set. `Deps.Client` must be uncached (Application events use field selectors).
patrol-server RBAC: `list` Events in Application namespaces; on destination clusters the
cluster credentials (or impersonated service accounts) need `get/patch/delete` on managed
kinds, `create` for create actions, `list` events, `get` pods and `pods/log`, `create`
`pods/exec`, and `impersonate` on service accounts when impersonation is used. HTTP servers
serving these routes must not set a `WriteTimeout`/`ReadTimeout` that cuts streams (SSE writes
set their own deadlines; WebSocket connections are hijacked).

## Invariants

- RBAC and existence hiding before any controller or cluster call; a resource outside the
  Application's tree is never read, patched, deleted, acted on, exec'd into or listed for
  events (membership from the controller, identity checked by group/kind/namespace/name and a
  live UID).
- No Secret data or sensitive annotation values in responses: live manifests, patch results,
  managed resources and server-side diffs are masked (masking decided from parsed objects);
  failed Secret patches return no destination message; logs and terminal content are never
  logged or audited; tokens never leave the request.
- Streams and sessions end when the client leaves, the session is revoked or expires, or
  permissions are lost; every destination stream is bound to the request context.
- Bounded: bodies (4 MiB default, 32 MiB server-side diff, 1 MiB actions), 5000 diff resources,
  100 impacted resources, 32 action parameters, 1000 events, tree stream buffer 16 events,
  log lines/bytes/rate, terminal message size, controller responses (200 MiB).

## Differences from upstream

| Upstream | Patrol | Reason |
|----------|--------|--------|
| Tree/managed resources/server-side diff from Redis, diff in the API server | From the owning controller replica (tree API) | ADR-0004 |
| `GET …/server-side-diff` with repeated query parameters | `POST` with a JSON body | contract; sizes |
| Server-side diff validates live states only: a target paired with a managed live resource may name any object, which the dry run applies with the controller's credentials | A target paired with a live state must have its group/kind/namespace/name (400); unpaired targets are not dry-run | tenant isolation (the CLIs pair by resource key, so valid requests match) |
| `POST …/resource/actions/v2` and deprecated `…/resource/actions` (action string body) | One route `POST …/resource/actions` with V2 semantics | contract |
| Disabled actions and undeclared parameters accepted | 412 / 400 | parameters validated against the action definition |
| Application self-action patched through the destination cluster configuration | Patched in the control plane (where Patrol Applications live), conditional on `resourceVersion`, project change refused | correctness; project change rules |
| Action also records an Event on the managed resource | Event on the Application only (audit) | audit Events target Applications/AppProjects |
| Terminal: any WebSocket origin; RBAC failures 401; wrong project 400 | Same-origin only; 403; 404 (existence hiding) | CSWSH; TEN-081 |
| Terminal token check on every message; no idle timeout; no audit | Permissions on every message, session/Application every 30 s; idle timeout; start/end audit | cost; threat model |
| SPDY with WebSocket fallback | WebSocket only (v5/v4) | module set |
| Unbounded log lines and rate; a line without timestamp ends the stream | 64 KiB lines, 2000 lines/s, 256 MiB; previous timestamp kept | bounded resources |
| SSE frames `{"result": …}` (gRPC gateway) | bare JSON events | JSON/HTTP API (ADR-0005) |
| Events unbounded | ≤ 1000 | bounded responses |

## Gaps

- Wiring belongs to the server assembly (`internal/server`, another task: `resourceDeps`,
  tree API client, and a controller tree API handler over a real `StateProvider` with
  server-side dry runs, `internal/server/treestate.go`). This component's tests still use the
  real tree API handler and client in front of a fake `StateProvider`.
- Request to the tree API owner (`internal/controller/treeapi`, `handler.go` `serverSideDiff`):
  enforce the same target/live identity check as `validateServerSideDiffPairs` (a target paired
  with a present live state must have the live resource's group/kind/namespace/name) so the
  controller does not rely on the API server for it. Today the handler validates the live side
  only.
- Upstream's resource-level Event for actions and the terminal "reconnect on token refresh"
  message are not implemented (sessions end instead).
- `patrol app resources|logs|actions|get-resource|patch-resource|delete-resource` CLI commands
  and the UI are other tasks.

## Parity IDs

Recorded in [`docs/parity/status/api-app-resources.yaml`](../../parity/status/api-app-resources.yaml):
implemented API-004 (logs part), API-021, API-022, API-023, API-024, API-026 (events part),
API-044, SYNC-100..103, SYNC-112, SYNC-113, SYNC-115..124, TEN-070, TEN-071; in-progress
DIFF-088 (CLI), TEN-084 (extensions part elsewhere).

## Verification

Run on 2026-10-16 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0):

```bash
go build ./...                                                                    # ok
go vet ./internal/apiserver/services/applications/ ./pkg/apiclient/              # ok
golangci-lint run ./internal/apiserver/services/applications/... ./pkg/apiclient/...   # 0 issues
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 ./internal/apiserver/services/applications/ ./pkg/apiclient/
# ok (both packages; no test skipped: 0 "--- SKIP" lines with -v); repeated 5 times after the
# review fixes, 5/5 ok
KUBEBUILDER_ASSETS=... go test -race -count=20 -run 'TestResourceTreeCacheMiss$' ./internal/apiserver/services/applications/
# ok (the stand-in controller race behind an earlier 1-in-5 failure is fixed)
go run ./hack/parity                                                              # exit 0
go run ./internal/apiserver/cmd/genopenapi -check
# api/openapi/openapi.json is stale (expected: the orchestrator regenerates it; the
# document generates without errors, TestResourceRoutesOpenAPI)
```

Review follow-up regression check: with the fixes reverted (server-side diff pair validation
not called, `maxPodLogsToRender` 0 treated as unlimited, the stand-in controller clearing the
annotation 300 ms before storing the state), `TestServerSideDiff`,
`TestApplicationLogsAuthorization` and `TestResourceTreeCacheMiss` failed; with the fixes all
three pass.

Tests of this component (`internal/apiserver/services/applications`), 22 through
`pkg/apiclient` against the real route chain (SSO sessions from `auth.TokenService`,
`rbac.Enforcer`, audit with a Kubernetes Event recorder), envtest as control plane and
destination cluster (reached through a recording proxy that forwards impersonation headers, so
Kubernetes RBAC applies to impersonated service accounts), the real tree API handler and
client over a fake `StateProvider`, the real project lookups and Lua runtime, and a kubelet
stand-in for `pods/log` and `pods/exec` (envtest has no kubelet):
tree (hiding matrix, 401), cache miss with a stand-in controller (which, like the real one,
stores the state before clearing the refresh annotation) and with a comparison error,
tree stream (ADDED, MODIFIED on change, 403 after a project move, DELETED, denial before any
event), managed resources (Secret data and sensitive annotations masked, hook excluded,
filters), server-side diff (masking, membership 403, spoofed identity 400, alignment, foreign
targets paired with a managed resource (other namespace/name/kind/group, missing namespace,
invalid JSON) refused with 400 and no dry run, matching pair and unpaired target diffed), events
(application and resource, tree/UID membership, denial), live get (Secret masking, orphaned
nodes, outside the tree, unserved version, resource links through the adapter), patch
(fine-grained allow/deny per kind, plain update refused by default and allowed with
inheritance, audit, JSON/merge/strategic, masked Secret result, Secret failure detail), delete
(fine-grained, propagation bodies), impersonation (enforced no-match 412 with no cluster call,
application events unaffected, Kubernetes 403 for the impersonated service account then access
after a RoleBinding, user-only headers, patch/delete impersonated, not enforced fallback,
disable without restart), actions (built-in discovery and disabled flags, custom actions
replacing built-ins, restart, parameters, script errors, disabled/unknown actions, RBAC per
kind/tenant, create-job with project blacklist refusal and dry run first, impersonated dry-run
denial with nothing created, Application toggle-auto-sync and the project-change refusal),
logs (Deployment aggregation merged by timestamp, filters, pod route options, untilTime,
outsider pod, pod stream error line, download, logs-vs-get permission, maxPodLogsToRender
(1 and 0: 0 refuses a single pod with no cluster call, no matching pod is an empty stream),
follow with clean cancellation of the pod stream, impersonated log requests, permission
revoked mid-stream 403, session revoked mid-stream 401), terminal (shell fallback, resize,
stdin, exit, audit without content, requested shell, no impersonation; exec disabled 404,
reader/logger/other tenant 403, anonymous 401, pod outside the tree, container not running,
wrong project 404, parameter validation, disabled namespace, forged origin 403, same-origin and
configured-url origins accepted, denials audited; exec permission revoked mid-session, session
revoked mid-session, idle timeout; enforced impersonation without match still allows the
terminal while logs fail). 21 unit tests: route table and access metadata, 503 without
collaborators, OpenAPI generation, pod selection (owners, cycles), log query parsing, filters,
line reading/truncation, merge ordering and quiet streams, status patch split, action parameter
validation, patch types, delete options, terminal parameters and origin rule, container and
shell selection, masking (spoofed kinds, fail closed), tree and destination error mapping, exec
URLs and status errors, download file names. `pkg/apiclient`: request encoding of every
method, log stream reading and downloads, `ApplicationTree.FindNode`, the terminal client
against a WebSocket server (frames, close, pre-upgrade problems).
