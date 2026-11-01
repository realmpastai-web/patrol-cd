# Component: application API (`internal/apiserver/services/applications`)

Status: implemented (core application routes; not yet wired into the `patrol-server`
command). Normative contract: [`api-contract.md`](../api-contract.md#applications). Framework:
[`apiserver.md`](apiserver.md). Decision records: [ADR-0005](../adr/0005-public-api.md),
[ADR-0009](../adr/0009-authn-authz.md), [ADR-0011](../adr/0011-operations-and-sync-state-machine.md),
[ADR-0013](../adr/0013-validation-strategy.md). Upstream baseline: Argo CD v3.5.5
`server/application/application.go`, `server/deeplinks`, `util/argo` (behavior re-implemented,
no code copied).

| Package | Contents |
|---------|----------|
| `internal/apiserver/services/applications` | `New(Deps) *Service` (implements `apiserver.Service`), route table (`routes.go`, with the extension point for the resource-level routes), handlers, `Broadcaster` (Application event fan-out), `ConfigFromParams` |
| `pkg/apiclient/applications.go` | Route constants, request/response types shared with the server, Go client methods (`ListApplications`, `WatchApplications`, `GetApplication`, `CreateApplication`, `UpdateApplication`, `UpdateApplicationSpec`, `PatchApplication`, `DeleteApplication`, `SyncApplication`, `RollbackApplication`, `TerminateApplicationOperation`, `ListApplicationOperations`, `GetApplicationOperation`, `GetApplicationSyncWindows`, `GetApplicationRevision{Metadata,ChartDetails,OCIMetadata}`, `GetApplicationManifests`, `GetApplicationManifestsWithFiles`, `ListApplicationLinks`, `ListApplicationResourceLinks`) |

Deep links use the shared evaluator `internal/apiserver/services/links` (created by the
project/cluster API work; see [`api-clusters-projects.md`](api-clusters-projects.md)).

## Routes and RBAC

Every route is `RBACInHandler(applications, <action>, "<project>/<app>")`: the handler reads
the Application and authorizes the object `<project>/<name>` (control-plane namespace) or
`<project>/<namespace>/<name>` (other namespaces, `?appNamespace=`), with `rest.Authorize`
(denials audited) or `rest.Can` (list/watch filtering, not audited).

| Method and path (`/api/v1`) | RBAC | Behavior |
|------|------|----------|
| `GET /applications` | get, per item | Enabled-namespace Applications, filters `projects`/`project` (legacy wins), `selector`, `repo` (any source), `name`, `appNamespace`; sorted by name then namespace; `limit` (≤ 5000) + opaque `continue`; `metadata.resourceVersion` of the list read |
| `GET /stream/applications` | get, per event | SSE `{type, object}`; initial `ADDED` (sorted by qualified name) without `resourceVersion` or for a named Application; filters as above plus `resourceVersion` (older objects skipped); visibility lost (project move, labels, permission) → `DELETED` carrying the last state the caller was allowed to see; slow subscriber → `event: error` 429 |
| `POST /applications` | create (+update on upsert) | Body Application (`?upsert`, `?validate` default true, `?appNamespace` when the body has none); metadata a client cannot set is cleared; identical existing → returned; different → 400 `existing application spec is different, use upsert flag to force update`; name owned by an Application the caller cannot get → 409 without content; a create that loses a race to a concurrent create applies the project change rules (update on the winner's project) before the upsert |
| `GET /applications/{name}` | get | Existence hiding (below); `?refresh=normal|hard` (anything else = normal) merge-patches `cd.patrol.example/refresh` (and `hydrate` for hydrator Applications), hard refresh asks the repo-server for app details without cache, then waits for a newer object without the annotation (events + 1 s polling), at most `RefreshTimeout` (15 s) → 504 `application refresh deadline exceeded`; client disconnect ends the wait; always audited |
| `PUT /applications/{name}` | update on the body's object, then update on the stored one | Spec, labels, annotations, finalizers replaced; status ignored; body `metadata.resourceVersion` → conditional (409); otherwise up to 10 conflict retries while the project is unchanged |
| `PUT /applications/{name}/spec` | update | Spec only: labels, annotations and finalizers of the stored object are never written, so concurrent metadata changes (e.g. the cascade finalizer of an in-flight delete) survive; conflicts re-apply only the spec (up to 10, project unchanged); returns the stored spec |
| `PATCH /applications/{name}` | get, then update | `{patch, patchType: json (default)|merge}` on the Application JSON, then the update path with validation; name/namespace immutable; a patch setting `resourceVersion` is conditional, otherwise conflicts re-apply the patch; JSON patches ≤ 1000 operations, `copy` operations add ≤ 1 MiB, patched document ≤ 4 MiB, else 413 `resource_exhausted` |
| `DELETE /applications/{name}` | get, then delete | `cascade` (default true) adds the finalizer of `propagationPolicy` (`""`/`foreground` → `resources-finalizer.cd.patrol.example`/`…/foreground`, `background` → `…/background`, case-insensitive) unless deleting; `cascade=false` removes all three; optimistic-lock merge patch, then delete with a UID precondition |
| `POST /applications/{name}/sync` | get, sync (+override) | Creates an Operation (below) |
| `POST /applications/{name}/rollback` | sync | Creates a rollback Operation |
| `DELETE /applications/{name}/operation` | sync | Merge-patches `spec.cancel=true` on every non-terminal Operation; none → 400 `Unable to terminate operation. No operation is in progress` |
| `GET /applications/{name}/operations` | get | Operations of the Application (`spec.application`, matching controller owner UID when present), newest first, `limit` (default 100, ≤ 1000) + `continue` |
| `GET /applications/{name}/operations/{op}` | get | One Operation; another Application's → 404 |
| `GET /applications/{name}/syncwindows` | get | `{activeWindows, assignedWindows, canSync}` (manual evaluation now) plus `status` and `nextTransition` |
| `GET /applications/{name}/revisions/{revision}/metadata` | get | Repo-server commit metadata with the project's effective source integrity; `?sourceIndex` (0-based), `?versionId` (history entry) |
| `GET …/revisions/{revision}/chartdetails`, `…/ocimetadata` | get | Helm chart / OCI metadata of the selected source (`no chart found for application: <app>`) |
| `GET /applications/{name}/manifests` | get | Repo-server rendering at `?revision` (single source) or `?revisions`+`?sourcePositions` (multi-source, equal lengths, 1-based); hydrator Applications render the dry source; `?noCache`; renders with the destination's version and API versions (below), and answers 503 `error getting the destination's versions: ...` when they are not in the Cluster status and the destination does not give them, whatever it answers |
| `POST /applications/{name}/manifests-with-files` | get + override | Body: gzipped tar (`application/gzip`), ≤ 32 MiB (`MaxLocalFilesBytes`), optional `?checksum` (hex SHA-256); single-source Applications; extraction limits are the repo-server's; the destination's versions as in `manifests` |
| `GET /applications/{name}/links` | get | `application.links` with `app`/`application`, `cluster` (identity fields only), `managedByURL` (annotation, else settings `url`); destination not permitted → 400 `error getting destination cluster` |
| `GET /applications/{name}/resource/links` | get | `resource.links` for the Application itself (`group=cd.patrol.example&kind=Application&resourceName=<app>`) or a managed resource read through `Deps.LiveResources` (Secret data masked; not in the tree → 400 `<Kind> <group> <name> not found as part of application <app>`; no LiveResources → 503) |

### Existence hiding (upstream getAppEnforceRBAC, TEN-081)

- With `?project=`: a denial on `<project>/<app>` → 403 (after reading the object anyway to
  equalize timing); missing → 404; denied on the Application's real project or another
  project → 404.
- Without a project: missing, forbidden and namespace-not-enabled → the same 403 `permission
  denied`.
- The effective project (`internal/project.ResolveAppProject`: scoped repositories and
  clusters, global projects, `sourceNamespaces`) must resolve; every failure is the vague 400
  `app is not allowed in project "<p>", or the project does not exist`.
- Mutation-specific checks (update, delete, sync, override, create) return the framework's
  detailed 403 `permission denied: applications, <action>, <object>, sub: …, iat: …` for an
  object the caller has already been shown or supplied.

### Sync (POST /sync)

Order: read with `get` (hiding rules) → `sync` → sync windows (manual, now; invalid window →
403 `cannot sync: invalid sync window: …`, blocked → 403 `cannot sync: blocked by sync window`)
→ local `manifests`: `override`, ≤ 1024 documents and 1 MiB total (413), automated sync only
for dry runs (412 `cannot use local sync when Automatic Sync Policy is enabled unless for dry
run`) → deleting → 412 `application is deleting` → resources (kind and name), infos (≤ 64,
named), retry backoff durations → source overrides (Patrol addition: `source` for single,
`sources` for multi-source; project permission of the override, `override` when it differs
from the spec, automated only for dry runs) → revisions: an override of the target revision
(empty target compared as `HEAD`) needs `override` when
`application.sync.requireOverridePrivilegeForRevisionSync` is true and is refused for automated
Applications unless dry run (412 `Cannot sync to <rev>: auto-sync currently set to <target>` /
`Cannot sync source <repo> to <rev>: …`); multi-source `revisions`/`sourcePositions` (equal
lengths, positions 1..n, `source position is out of range`); each revision is resolved by the
repo-server (`ForceRefresh`, credentials of the project) unless it is a Git commit SHA of a
non-Helm source or local manifests are synced; resolution failures → 412 with the repo-server
message, repo-server outage → 503 → effective options: request `retryStrategy` else spec
retry, request `syncOptions.items` (when present, even empty) else spec options; options are
validated (`project.ValidateSyncOptions`) → `Replace=true` refused when
`server.sync.replace.allowed=false` (412) → local manifests refused under source integrity
criteria (412) → a non-terminal Operation of the Application → 409 `failed_precondition`
`another operation is already in progress` → create.

The Operation: `generateName: <app>-sync-` (`-rollback-`), owner reference (controller) to
the Application, label `cd.patrol.example/application` when the name is a valid label value,
`spec.application`, `spec.sync` (`source` for single-source / `sources` for multi-source from
the spec or the override, `revision(s)` resolved, `prune`, `dryRun`, `syncStrategy`,
`resources`, `manifests`, `syncOptions`), `spec.initiatedBy.username` = the verified
principal's user name (account, SSO email or subject; empty for anonymous), `spec.info`,
`spec.retry`. Response `{application, operation}`; local manifests in the returned Operation
have Secret data masked. Event `OperationStarted` `<user> initiated [partial ]sync to <rev>` /
`initiated [partial ]sync locally`.

Rollback: read with `sync`; deleting → 412; automated (enabled) → 412 `rollback cannot be
initiated when auto-sync is enabled`; unknown id → 400 `application <ns/name> does not have
deployment with id <id>`; entry without source(s) → 412 `cannot rollback to a deployment
without recorded sources. sync to revision instead.`; multi-source Application and
single-source entry → 412; then the in-progress check and an Operation with
`spec.rollback.id`, `spec.sync{prune, dryRun, syncStrategy.apply, syncOptions (spec)}`, no
retry; event `initiated rollback to <id>`.

### Resource health from the tree (DIFF-064, APP-031)

With `controller.resource.health.persist=false` (the default) the controller stores no
`status.resources[].health` and sets `status.resourceHealthSource: appTree`; the health lives in
the controller's resource tree. `withTreeHealth` (`health.go`, upstream
`inferResourcesStatusHealth`) fills it into a copy of the Application for
`GET /applications/{name}` (with and without `refresh`), the `ADDED`/`MODIFIED` events of
`GET /stream/applications`, and the Application of the sync, rollback and terminate responses. `GET
/applications` does not fill it (upstream List).

- Only Applications with `resourceHealthSource: appTree` and at least one resource are read;
  inline health is returned as stored.
- One `TreeClient.Tree` call per response or event, bounded by
  `ResourceDeps.ResourceHealthTimeout` (5 s). Each resource gets the health of the managed tree
  node with its group, kind, namespace and name; a node without health, UID, resourceVersion
  and createdAt gives `Missing` / `Resource has not been created`; no node gives nil. Orphaned
  nodes do not count.
- A failed read leaves the health nil and the response is served anyway. The tree is read
  directly, never through `withAppState`, so a controller without state for the Application
  never triggers a refresh.
- A watch stream whose read failed without an answer from the controller (transport error,
  timeout, routing error) stops reading for 30 s; an answer such as "not compared yet" concerns
  one Application and does not pause.
- The fill never writes the Application and never modifies its argument (watch events share
  one object between subscribers).

## Wiring (`Deps`)

| Field | Production value |
|-------|------------------|
| `Config` | `ConfigFromParams(namespace, params)` of the server component (`application.namespaces`, `server.sync.replace.allowed`); `DisableGPG` from `PATROL_GPG_ENABLED=false` |
| `Client` | controller-runtime client (uncached reads, writes). The server's identity needs get/list/watch on applications, appprojects, clusters, repositories, operations; create/update/patch on applications and operations; **the `impersonate` verb on `operations.cd.patrol.example`** (`v1alpha1.OperationInitiatorVerb`), otherwise the Operation admission policy rejects `spec.initiatedBy` and syncs fail with 500 (logged). Cluster-scoped permissions are only needed with `application.namespaces` |
| `Reader` | the manager's cache (lists, project/cluster/repository lookups); writes then wait up to 2 s for the cache (upstream `waitSync`) |
| `Settings` | `*settings.Manager` |
| `Events` | `NewBroadcaster(0)` added to the cache's Application informer (`inf.AddEventHandler(b)`) |
| `Repositories` | `*repository.Resolver` |
| `RepoServer` | the repo-server HTTP client |
| `LiveResources` | the controller tree API adapter (resource routes task); nil → resource links of managed resources answer 503 |

## Invariants

- Authorization before any write; reads that reveal existence follow the hiding rules; lists
  and streams filter per item and per event, with the Application state of that event.
- No secrets in responses or logs: repository credentials go only to the repo-server; Secret
  `data`/`stringData` and `resource.sensitive.mask.annotations` are masked
  (`internal/diff.HideSecretData`) in rendered manifests, local manifests of returned
  Operations and resource-link contexts; a manifest that cannot be parsed for masking is
  replaced (fail closed); repository URLs with user information are redacted in messages.
- Repositories the project does not permit are never rendered or queried (kubectl-applied
  Applications are not validated): 403 `application repo <url> is not permitted in project
  '<p>'`.
- Bounded inputs: route bodies (4 MiB default, 3 MiB sync, 32 MiB local files), list limits,
  1 MiB local manifests, refresh wait 15 s, watch buffer 1000 events per subscriber, continue
  tokens ≤ 4 KiB, JSON patches (1000 operations, 1 MiB added by `copy`, 4 MiB result: the
  body cap alone does not bound a patch whose copies double the document).
- Optimistic concurrency on every write (every update carries the resourceVersion it was
  computed from; retries re-read and only proceed when the authorized project and UID are
  unchanged; finalizer patches with optimistic lock; delete with UID precondition). What a
  retry re-applies depends on the route: PATCH re-applies the patch to the fresh object,
  `/spec` only the spec (fresh metadata kept), PUT without a body resourceVersion and upsert
  the client's labels/annotations/finalizers (last writer wins on what the client sent, as
  upstream; clients that must not overwrite concurrent metadata send `resourceVersion`).
- Project change rules (create on the new, update on the old RBAC object) hold on every path
  that can move an Application, including an upsert whose create lost a race.
- Mutations are audited with the Application as target (Kubernetes Events
  `ResourceCreated|Updated|Deleted`, `OperationStarted`); refreshes are audited.
- Handlers only touch control-plane objects and the repo-server; the controller reconciles
  asynchronously. No URL given by a user is fetched by these handlers (managed-by URLs are only
  validated; repository access is the repo-server's, with its egress rules).

## Differences from upstream

| Upstream | Patrol | Reason |
|----------|--------|--------|
| `Application.operation` set by `SetAppOperation` | Operation objects; responses `{application, operation}`; in-progress → 409 `failed_precondition` | ADR-0011 |
| Terminate sets `status.operationState.phase=Terminating` | `spec.cancel=true` on the Operation | ADR-0011 |
| Sync windows checked before the `sync` permission | `sync` first | a caller without sync learns nothing |
| Sync options unchecked | validated (`project.ValidateSyncOptions`) | ADR-0013 |
| `sync.source(s)` only from spec | optional `source`/`sources` override (override permission, project permission) | contract: body mirrors `Operation.spec.sync` |
| Rollback message mentions "Argo CD v0.11" | `cannot rollback to a deployment without recorded sources…`; multi-source Applications need multi-source entries | no Argo CD history format |
| Hydrate annotation set on every refresh | only for hydrator Applications | upstream leaves it forever on others |
| Refresh waits until the client deadline | at most 15 s, then 504 | bounded requests |
| `GetManifestsWithFiles` client stream | gzipped tar body, optional checksum, requires `override` | no gRPC; local content is like a local sync |
| Manifests: cluster version and API versions from live discovery on every call | from `Cluster.status` (`serverVersion`, `info.apiVersions`), which the controller keeps, and from the destination only when the status lacks one (the implicit in-cluster cluster, a Cluster with no Applications yet) | a preview should not cost a discovery of the destination; a destination the project does not permit is not contacted, nor, in core mode, one the kubeconfig has no context for: the render then has what the status holds |
| List: no pagination, repo filter on the first source | `limit`/`continue`; any source | contract; multi-source |
| Create of a taken name returns the existing object or a spec diff error | 409 without content when the caller cannot get the existing Application | no cross-project disclosure |
| Validation lookup failures reported as InvalidArgument | 503 `error validating project permissions` | not the caller's error |
| Deep link URLs not filtered | only absolute http(s) URLs (links package) | threat model T15 |
| Local manifests visible in `operationState` | masked in API responses | threat model T14 |
| `UpdateSpec` copies labels/annotations/finalizers of its first read on every retry (`updateApp`) | `/spec` writes only the spec | concurrent cascade delete must keep its finalizer (APP-051) |
| Upsert after a lost create race checks `update` only on the new object | project change rules applied to the winning Application | TEN-043 / APP-055 |
| JSON patch `copy` accumulation unlimited, no operation cap | 1000 operations, 1 MiB copied, 4 MiB result (413) | memory exhaustion from a sub-KiB body |
| Resource health read from the Redis tree cache | read from the controller tree API, bounded (5 s) | no shared cache; the controller owns the tree |
| Sync/Rollback fill a copy that `SetAppOperation`'s result replaces, so success responses carry no resource health; Terminate returns no Application | the Application of the sync, rollback and terminate responses carries it | responses are `{application, operation}` (ADR-0011), and the UI writes their Application into its detail cache |
| Every watch event is filled | `DELETED` events are not | the controller drops the state of deleted Applications |
| Watch reads the cache for every event | a stream stops reading for 30 s after the controller did not answer | a remote call per event must not stall a stream |

## Gaps

- Not wired into `cmd/patrolcd` (server assembly, RBAC manifests with the impersonate verb).
- The project read lock around create/update (upstream `projectLock`) is not implemented.
- Resource links of managed resources need the `LiveResources` implementation of the resource
  routes task; resource-level routes (tree, managed resources, server-side diff, events, live
  resources, actions, logs, terminal) are that task's.
- `fields=` projection of lists (API-003) is not supported.

## Parity IDs

Recorded in [`docs/parity/status/api-applications.yaml`](../../parity/status/api-applications.yaml):
implemented APP-049..054, APP-056, TEN-036, TEN-043, TEN-069, TEN-081, SYNC-002, SYNC-005,
SYNC-047, API-017..020, API-025, DIFF-064 (alias APP-031; API part); in-progress (API part done) APP-067, APP-069, APP-080, APP-102,
APP-141, TEN-010, TEN-040, TEN-068, SYNC-001, SYNC-003, SYNC-004, SYNC-011..014, SYNC-016,
SYNC-078, SYNC-087, SYNC-088, SYNC-091, API-026, API-027.

## Verification

Run on 2026-10-16 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0), after the review
fixes (upsert race, `/spec` metadata, JSON patch bounds):

```bash
go build ./...    # fails only in internal/controller/sharding (another task's package, lease.go: m.ticked undefined)
go build $(go list ./... | grep -v internal/controller/sharding)         # ok
go vet ./internal/apiserver/services/applications/ ./pkg/apiclient/       # ok
golangci-lint run ./internal/apiserver/services/applications/... ./pkg/apiclient/...   # 0 issues
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 ./internal/apiserver/services/applications/ ./pkg/apiclient/
# ok (both packages; every envtest test ran, none skipped)
go run ./hack/parity                                                      # exit 0
```

Tests (`internal/apiserver/services/applications`): through `pkg/apiclient` against the real
route chain (`auth.TokenService` SSO and project-role tokens, `rbac.Enforcer` with project
policies, audit with a `record.FakeRecorder`), envtest with the generated CRDs, the real
`internal/project` lookups and `internal/repository` resolver (Repository + labelled Secret →
credentials reach the fake repo-server, never a response), an Application informer feeding the
`Broadcaster`, and an in-process fake repo-server: create/upsert/validation messages, denials
for other tenants, readers, anonymous and project tokens, the existence-hiding matrix, lists
(filters, pagination, per-item RBAC), update/patch/spec (conflicts, status ignored, project
change rules), delete (finalizers, errors), refresh (stand-in controller, timeout with and
without events), apps in any namespace (enabled namespaces, sourceNamespaces, namespace-scoped
policy), cached reads, watch (initial state, tenant isolation, project move → DELETED,
selector, errors), sync (Operation content, resolution, one at a time, options, local
manifests and masking, override rules, windows, deleting, replace gate, multi-source, source
overrides, unavailable repo-server), the Operation admission policy with an impersonated
service account (refused without the impersonate verb, admitted with it), rollback, terminate,
operations list/get, sync windows, revision metadata, manifests (masking, unpermitted
repository, multi-source refs, hydrator, the destination's versions from the status or the
destination), manifests-with-files (override, checksum, gzip, size, content type), links (conditions, managed-by URL, Application and masked Secret
resource links, destination permission), races driven by a repo-server hook that changes the
cluster while a handler validates (an upsert whose create loses to a concurrent create in
another project is denied without update there; `/spec` keeps labels, annotations and
finalizers set concurrently, including the cascade finalizer of an in-flight delete), JSON
patch bounds through the API (exponential `copy`, operation count, nothing written); unit
tests for the route table (every contract route,
RBAC metadata, 503 without dependencies), OpenAPI generation of the routes, namespace
matching, masking, patches and their bounds (413 and bounded allocation for a rejected
40-copy patch), retry validation, source selection, the broadcaster and
`ConfigFromParams`. `pkg/apiclient`: request encoding of the client methods and the watch
stream.

51 tests and 45 subtests in the package, none skipped.

Known: `internal/apiserver` `TestOpenAPIUpToDate` fails with `api/openapi/openapi.json is
stale` until the orchestrator regenerates the document with the new services (expected; the
document generates without errors).

Resource health from the tree (issue #11), run on 2026-10-29 (macOS arm64, Go 1.27.3, envtest
kube-apiserver 1.36.0); the exact commands and results, with the later runs and the review's
mutation checks, are V-0044 in [`docs/verification/log.md`](../../verification/log.md):

```bash
go vet ./internal/apiserver/services/applications/ ./internal/cli/app/ ./internal/compare/   # ok
golangci-lint run ./internal/apiserver/services/applications/... ./internal/cli/app/... ./internal/compare/...   # 0 issues
go test -race -count=1 -run 'TestTreeHealthByKey|TestWithTreeHealth|TestPausesTreeHealth' \
  ./internal/apiserver/services/applications/                                 # ok
KUBEBUILDER_ASSETS=<envtest 1.36.0 assets> PATROL_REQUIRE_ENVTEST=1 \
  go test -race -count=1 ./internal/apiserver/services/applications/          # ok (53.9 s)
KUBEBUILDER_ASSETS=<envtest 1.36.0 assets> PATROL_REQUIRE_ENVTEST=1 \
  go test -race -count=1 ./internal/cli/app/                                  # ok (41.1 s)
KUBEBUILDER_ASSETS=<envtest 1.36.0 assets> PATROL_REQUIRE_ENVTEST=1 \
  go test -race -count=1 ./internal/compare/                                  # ok (6.7 s)
go run ./internal/apiserver/cmd/genopenapi -check                             # exit 0 (no route change)
go run ./hack/parity && go run ./hack/parity -check                           # exit 0
```

Without the change (`go test -overlay` with the previous `crud.go`, `watch.go` and
`operations.go`), `TestApplicationResourceHealthFromTree` fails in get, refresh, watch, sync,
rollback, unavailable, slow controller and watch deleted (list and inline pass: they guard
upstream List and inline health), and `TestE2EAppResourceHealthFromTree` fails (empty HEALTH
column, then `app wait --health --resource :ConfigMap:cfg` times out after 15 s). With the
watch pause disabled the slow-controller subtest counted 6 tree reads instead of 1 (its first
version; the later mutants of the pause are in V-0044); with a fill
that writes into its argument `TestWithTreeHealth` fails and `-race` reports a data race in the
watch subtest; with `DELETED` events filled too, the watch deleted subtest fails (the event
carries the tree's health and the tree is read once more).
