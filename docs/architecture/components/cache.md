# Component: `internal/cache`

Status: implemented (phase 1, library only). Owner task: T1.11. Decision records: ADR-0004
(no Redis; the controller owns the live object graph), ADR-0001 (independent implementation).

`internal/cache` is the controller's per-cluster live-state cache. It discovers the APIs a
destination cluster serves, lists and watches them, and keeps a compact, owner-indexed graph
of every in-scope object in memory. It holds no authoritative state: a cache can be dropped
and rebuilt from the API server at any time. Behavior was researched in upstream
`gitops-engine/pkg/cache` and `controller/cache` (Argo CD v3.5.5) and re-implemented; no
upstream code is imported or copied.

The package does not import `api/v1alpha1` or any other Patrol package.

## Responsibilities

- Discovery of every served API, and selection of the preferred version of each group/kind
  that supports `list` and `watch` and is not excluded.
- Paginated initial lists (retry with jittered backoff on retryable errors, restart on an
  expired continue token), then `resourceVersion`-based watches with bookmarks.
- Relist on 410 Gone/Expired, periodic relist per API (`WatchResyncTimeout`), jittered
  backoff on watch errors, periodic full resync (`ClusterResyncPeriod`, run in the background).
- Reaction to CRD and APIService changes: watches start and stop without a full resync.
- Namespaced mode: per-namespace lists and watches, cluster-scoped APIs only with
  `ClusterResources`.
- `respectRBAC` (`none`, `normal`, `strict` with SelfSubjectAccessReview). APIs that cannot
  be watched (403 on watch) fall back to periodic relists and are reported in `ClusterInfo`.
- Compact records: owner references, tracking label/annotation values, a caller-computed
  `Info`. A full manifest is kept only for managed objects and CRDs, without
  `managedFields` and with the last-applied annotation moved to its own field.
- Owner hierarchy (UID-based, cluster-scoped owners of namespaced objects, and the upstream
  inferred relations) and live-object retrieval for comparison (`GetManagedLiveObjs`).
- Lazy OpenAPI v3 schemas and a structured-merge-diff type resolver for diffing.
- A `Manager` that owns one cache per destination cluster.

Out of scope: health assessment, resource tracking/app attribution, ignore-resource-updates
hashing and node info enrichment. They plug in through `Settings.PopulateResourceInfo` and
`OnResourceUpdated` (owned by `internal/health`, `internal/tracking` and
`internal/controller`). Also out of scope: building `rest.Config` (`internal/clusters`),
reading settings (`internal/settings`), shard assignment (`internal/controller`).

## Public API

| Symbol | Purpose |
|--------|---------|
| `New(cfg *rest.Config, Settings, ...Option) (*Cluster, error)` | Creates a cache. It does not contact the cluster until `EnsureSynced`. `Close()` stops it. Options: `WithLogger(*slog.Logger)`, `WithListerWatcherWrapper(fn)` (fault injection in tests). |
| `ClusterCache` interface (implemented by `*Cluster`) | `EnsureSynced(ctx)`, `Invalidate(...InvalidateOption)`, `GetServerVersion()`, `GetAPIResources()`, `IsNamespaced(gk)`, `FindResources(ns, preds...)`, `IterateHierarchy(keys, fn)`, `GetManagedLiveObjs(ctx, targets, isManaged)`, `OnResourceUpdated`, `OnEvent`, `OnProcessEvents`, `GetClusterInfo()`, `GetOpenAPISchema(ctx)`, `GetGVKParser(ctx)`. |
| `Settings` | `ResourceFilter` (`IsExcluded(group, kind, cluster)`), `Namespaces`, `ClusterResources`, `WatchResyncTimeout`, `ClusterResyncPeriod`, `SyncRetryTimeout`, `EventProcessingInterval`, `ListPageSize`, `ListSemaphore`, `ListRetryAttempts`, `RespectRBAC`, `PopulateResourceInfo(un, isRoot) (info, cacheManifest)`, `IsManaged(un)`, `TrackingLabelKeys`, `TrackingAnnotationKeys`, `RetainManagedFields`, `Metrics`. A zero value selects the default. A negative duration disables the behavior. |
| `Metrics` | Callbacks: `ActiveWatches(n)`, `EventsProcessed(n, d)`, `SyncCompleted(d, err)`, `Relisted(gk, ns, reason)`. |
| `InvalidateOption` | `WithSettings(Settings)`, `WithConfig(*rest.Config)`. |
| `Resource` | `Ref` (`ResourceKey` + `UID` + `APIVersion`), `OwnerRefs` (including inferred), `ResourceVersion`, `CreationTimestamp`, `Labels`/`Annotations` (tracking keys only), `Info`, `Resource` (retained manifest or nil), `LastAppliedConfiguration`. |
| `ResourceKey` | `{Group, Kind, Namespace, Name}`. `String()` gives `group/kind/namespace/name`, `ParseResourceKey`, `GroupKind()`, `KeyOf(un)`, `NewResourceKey`. |
| `ClusterInfo` | `Server`, `K8SVersion`, `APIsCount`, `ResourcesCount`, `LastCacheSyncTime`, `SyncError`, `Synced`, `ActiveWatches`, `UnavailableAPIs []UnavailableAPI{GroupKind, Version, Namespace, Reason, Message}` (reasons: `ListForbidden`, `WatchForbidden`, `DiscoveryFailed`). |
| `APIResourceInfo` | `GroupKind`, `GroupVersionResource`, `Meta metav1.APIResource`. |
| `DiscoverAPIResources(ctx, *rest.Config)` | The server version and API resources a cache of the cluster would report, for a caller with no cache (`GET .../manifests`). The discovery client takes no context, so `ctx` reaches its requests through the transport, and `discoveryTimeout` bounds them together. |
| `GVKParser` | `Type(gvk) *typed.ParseableType` (nil when unknown or ambiguous) and `Ambiguous()`. |
| `Manager` | `NewManager(ManagerOptions{Logger, OnCacheCreated, CacheOptions})`, `Get(ctx, clusterKey, cfg, settings) (ClusterCache, error)` (creates if absent, then `EnsureSynced`), `Lookup(clusterKey) (ClusterCache, bool)` (the existing cache, without creating or syncing one), `Invalidate(clusterKey)` (closes and forgets), `ClustersInfo()`, `Close()`. |
| Predicates | `TopLevelResource`, `ResourceOfGroupKind(group, kind)`. |
| Errors | `ErrClusterUnreachable` (`*UnreachableError{Server, Err}`), `ErrClosed`, `ErrNotSynced`, `ErrUnknownGroupKind`, `ErrOutOfScope` (`*OutOfScopeError`, upstream message text), `ErrInvalidResourceKey`, `ErrManagerClosed`. |

### Adapting CRD types (callers)

- `Cluster.spec` maps to `rest.Config` (built by `internal/clusters`). `spec.namespaces`
  maps to `Settings.Namespaces` and `spec.clusterResources` to `Settings.ClusterResources`.
  When any of them changes, or `refreshRequestedAt` is newer than
  `ClusterInfo.LastCacheSyncTime`, call `Manager.Invalidate(key)` (or
  `Cluster.Invalidate(WithConfig, WithSettings)` to keep handlers).
- `patrol-cm` maps as follows: `resource.exclusions`/`resource.inclusions` and the
  per-cluster filter go to `Settings.ResourceFilter`; `resource.respectRBAC` goes to
  `Settings.RespectRBAC` (`""` → `none`). The tracking label key and the tracking-id
  annotation key (`api/v1alpha1/keys.go`) go to `TrackingLabelKeys` and
  `TrackingAnnotationKeys`.
- Controller parity knobs (DIFF-101): pass `ClusterResyncPeriod: 12h` (Argo CD's controller
  default; the library default is gitops-engine's 24h). Map Argo's "0 disables" to a
  negative duration. Map `ATTEMPT_LIMIT=1` to `ListRetryAttempts: 1`. Batch processing
  cannot be turned off; `EventProcessingInterval < 0` applies events as soon as they arrive.
- Managed selection (DIFF-076): `PopulateResourceInfo` should return
  `cacheManifest = appName != ""` for root objects with tracking. CRD manifests are always
  retained by the cache.
- `GetManagedLiveObjs(ctx, targets, func(r) bool { return info(r).AppName == app })`.
  Server-side diff and `managedFieldsManagers` ignore rules need
  `RetainManagedFields: true`.

## Invariants

1. **Published namespace maps are immutable.** Every graph mutation happens in a
   write-locked section. The first mutation of a namespace in a section clones its map, and
   the section ends with `seal()`. A map obtained while the write lock is not held is never
   modified again. `FindResources`, `IterateHierarchy` callbacks and `OnResourceUpdated`
   handlers therefore run without locks. Property test: `TestGraphInvariantsProperty`.
2. **Resources are immutable.** Updates, including inferred StatefulSet→PVC references,
   replace the `*Resource` pointer. `GetManagedLiveObjs` returns deep copies.
3. **Ordering per watch scope.** Events and relist results of one (API, namespace) go
   through one FIFO queue to a single processor. Sync and refresh operations are applied
   before queued events. Items from a stopped or superseded watch registration (token
   mismatch) are dropped, so a relist can never be overwritten by older events.
4. **Indexes match the namespace maps.** Indexes: by group/kind, owner UID → children,
   UID-less owner key → children, PVCs per namespace, inferred parents. `checkGraph`
   recomputes them in property tests.
5. **`IterateHierarchy` visits each resource at most once.** It is depth-first,
   deterministic (children sorted by key) and cycle-safe. Namespaced parents only own
   children in their own namespace. Cluster-scoped parents can own children in any
   namespace. Property test: `TestHierarchyProperty`, against a naive model.
6. A sync result is valid only for the `Invalidate` epoch it started in. A sync that an
   `Invalidate` supersedes is retried, never installed.
7. Nothing is persisted. Every cache can be rebuilt by `EnsureSynced`.
8. **No orphaned objects.** Every cached object belongs to a live watch registration (until
   the next processor flush). A registration that stops (its API reported gone by a 404,
   dropped from discovery, or moved to another version or scope) has its objects removed.
   When its API is still served it is listed again from scratch under a new registration.
   A registration whose API is reported gone is dead at once: its objects are removed and
   its later events are dropped. A transient discovery failure of a group never stops that
   group's registrations.

## Concurrency and data flow

```
watch goroutine (one per API × scope)            processor goroutine (one per cluster)
  List (paged, semaphore) / Watch(rv, bookmarks)     ┌─ batch window (EventProcessingInterval)
  OnEvent handlers (raw)                             │  write lock: apply pending + batch,
  convert → *Resource (PopulateResourceInfo,         │  seal namespace maps, capture snapshots
  strip manifest)                                    │  unlock
  enqueue ──► bounded queue (1024) ─────────────────►└─ OnResourceUpdated / OnProcessEvents
refresh goroutine: CRD/APIService/404 → discovery → start/stop watches (pending ops)
sync goroutine (single flight, root context): discovery → parallel lists into a staging
  graph → swap; EnsureSynced callers wait on it with their own context
```

- A foreground full sync runs on a goroutine owned by the cache, under the cache's root
  context, and is shared by every concurrent `EnsureSynced` caller. A caller's context only
  bounds its wait. A caller that gives up does not cancel the sync, whose outcome is recorded
  for later calls. A caller never receives the result of a sync that completed for an
  `Invalidate` epoch older than the call; it waits for a newer sync instead.
- A full sync builds a staging graph without locks. Readers keep using the previous state
  until an O(1) swap under the write lock. After a resync (not the first sync), changes are
  computed by resourceVersion diff and delivered to `OnResourceUpdated`.
- `OnResourceUpdated` receives the namespace snapshot taken after the batch with the change
  was applied, so it can already show later changes from the same batch. Handlers run in
  order on one goroutine, and panics are recovered and logged. A slow handler applies
  backpressure: the queue fills and watchers block. Memory does not grow.
- `OnEvent` handlers run concurrently on the watch goroutines. They must not keep or mutate
  the object they receive.
- `Close` and `Manager.Invalidate` must not be called from a handler of the same cache,
  because they wait for the processor.

## Failure modes

| Condition | Behavior |
|-----------|----------|
| API server unreachable (transport error, DNS, timeout, 503/504) | `EnsureSynced` returns `*UnreachableError` (`errors.Is(err, ErrClusterUnreachable)`). Calls within `SyncRetryTimeout` (default 10s) return the same error. The next call after that retries. `Invalidate` forces an immediate retry. |
| Caller context ends during sync | The caller gets the context error (wrapped). The sync continues under the cache's context, its outcome is recorded, and later callers get it without a new sync. A context that is already done when a sync would be needed starts nothing. |
| `Close` during sync | Waiting callers get `ErrClosed`. |
| Partial discovery failure (some group versions) | Full sync: it continues without the failed groups (as upstream does). Failed group versions appear as `DiscoveryFailed` in `ClusterInfo.UnavailableAPIs`. If discovery returns no resource lists at all, the sync fails. API refresh: the failed groups are treated as unknown, not absent. Their watches and objects are kept, their previous discovery data (`IsNamespaced`, `GetAPIResources`, GET resolution) is carried over, and expectations about them count as unconfirmed, so the refresh is retried. |
| List 401/403 | `none`: the sync fails with the API error. `normal`: the scope is skipped and reported as `ListForbidden`. `strict`: an SSAR confirms the denial. If the SSAR allows access, the sync fails (as upstream does); if the SSAR request fails, the sync fails. `GetManagedLiveObjs` fetches targets in a `ListForbidden` scope with GET, and a 401/403 on that GET is returned as an error. |
| Watch 401/403 | Reported as `WatchForbidden`. The scope is relisted every `WatchResyncTimeout` (polling). The entry clears when a watch succeeds. |
| 410 Gone/Expired (watch request or error event) | Immediate relist, then watch from the new resourceVersion. |
| Expired continue token during pagination | The list restarts (at most 3 times). |
| Watch stream closed | Resumes from the last resourceVersion, including bookmarks. A stream that closes faster than 1s backs off. |
| Other watch/list errors | Jittered exponential backoff: watches 1s→30s, list pages 200ms→5s for at most `ListRetryAttempts`. |
| API removed (404, CRD deleted) | A watch or relist that gets 404 marks its registration dead and removes the API's objects at once (upstream semantics), then requests a discovery refresh. While discovery still serves the API (aggregated discovery lags), the refresh relists it. On another 404 the API stays unregistered and the refresh is retried until discovery agrees. Targets of an unregistered or dead API are fetched with GET. |
| New CRD or Available APIService | Discovery refresh, retried 9 times from 500ms (×1.5) until discovery serves the kind or group. |
| OpenAPI load failure | Returned to the caller of `GetOpenAPISchema`/`GetGVKParser` and retried on the next call. A group version that disappears during the load is skipped. |
| Periodic resync fails | The previous state and watches stay in place. `EnsureSynced` reports the error within the retry window. |

## Security notes

- Credentials stay in the `rest.Config` given by the caller. Logs include only the server
  URL, group/kind/namespace and error text, never tokens or certificates. API server
  deprecation warnings are suppressed.
- Destination cluster content is untrusted. Owner references can form arbitrary graphs, and
  traversal is cycle-safe and bounded by the objects present. Inference parses only a few
  fields defensively (no regular expressions are built from object names). Handler panics do
  not stop the processor.
- Memory and I/O bounds: list page size (500), concurrent lists (`ListSemaphore`, 50 per
  cluster; it also bounds sync goroutines), queue (1024 items), batch (1000 items), parallel
  GETs (8). A 32s client timeout applies to discovery, OpenAPI and SSAR when the config sets
  none. Watches have no client timeout. When unset, QPS/Burst default to 50/100.
  Full manifests are kept only for managed objects and CRDs, without `managedFields`.
- Least privilege: with `RespectRBAC` set, the cache works with credentials that cannot
  list everything, and reports what it cannot see instead of failing.

## Parity feature areas served

From `docs/parity/areas/04-diff-health-resources.md`:

- DIFF-067/068: exclusions and inclusions, applied through `ResourceFilter`.
- DIFF-069: always-excluded `events.k8s.io`, `metrics.k8s.io`, core `Event`,
  `coordination.k8s.io/Lease`. The cache enforces these itself.
- DIFF-071: `respectRBAC`.
- DIFF-072: discovery and the watched set.
- DIFF-073: CRD and APIService changes.
- DIFF-074: version conversion, by GET in the target version.
- DIFF-075: namespaced-mode constraints, with upstream error messages.
- DIFF-076: managed live object selection.
- DIFF-081/082: hierarchy traversal and cross-namespace children.
- DIFF-083 to 086: synthesized relations.
- DIFF-087: inputs for app attribution, via `OnResourceUpdated` with namespace snapshots.
- DIFF-101: tuning knobs.
- DIFF-102: server version and API list.

From `docs/parity/areas/03-repositories-clusters-webhooks.md`:

- REPO-079/080: `namespaces` and `clusterResources`.
- REPO-094: cache info (`resourcesCount`, `apisCount`, `lastCacheSyncTime`, server version,
  API versions).
- REPO-096: cache invalidation.

The cache also provides the OpenAPI/GVK data that DIFF-008 and DIFF-009 (structured-merge
and server-side diff) need.

## Known gaps and deliberate differences

- **OpenAPI is lazy and v3-only.** Upstream loads OpenAPI v2 (`openapi.Resources` and a
  `GvkParser` from proto models) during every sync and fails the sync on error. Here,
  OpenAPI v3 component schemas and a `GVKParser` (built with `schemaconv`) load on first
  use. A GVK published by two conflicting schemas resolves to nil instead of an arbitrary
  pick. kubectl `openapi.Resources` is not provided.
- **`managedFields` are dropped by default.** Server-side diff and `managedFieldsManagers`
  ignore rules (DIFF-009/021) need `RetainManagedFields: true`. Objects fetched by GET in
  `GetManagedLiveObjs` always keep them.
- **Version conversion uses GET.** Upstream converts locally with the kubectl scheme and
  falls back to GET. Here, a target in a version other than the cached one is always
  fetched. Results match; the cost is extra API calls.
- **StatefulSet→PVC matching is exact.** It requires `<template>-<sts>-<digits>`. Upstream
  uses an unanchored, unescaped regex (DIFF-085 edge case), so it can also match prefixed
  names and treat regex metacharacters specially.
- **SSAR attributes in strict mode.** The review sets the resource's API group and checks
  each namespace scope separately (`""` for cluster-wide). Upstream omits the group and uses
  namespace `*`, and in namespaced mode keeps the API if any namespace is allowed.
- **Forbidden APIs are retried less often.** APIs skipped for RBAC are not retried by CRD/API
  refreshes, only by the next full resync or `Invalidate`. Upstream retries them on every
  CRD event.
- **Handler snapshot semantics.** `IterateHierarchy` callbacks and update handlers run
  without locks on immutable snapshots. Upstream holds the cache lock. A handler's namespace
  snapshot is post-batch (see above).
- **List page prefetch.** `ARGOCD_CLUSTER_CACHE_LIST_PAGE_BUFFER_SIZE` is not implemented;
  pages are fetched one after another.
- **Forbidden relists keep the previous objects.** When a relist of a registered scope becomes
  forbidden (`ListForbidden` after a watch 403), the objects cached before stay until a
  later relist succeeds or a full resync runs. `GetManagedLiveObjs` fetches targets in that
  scope with GET, but its `isManaged` scan can still return those older copies.
- **`Close` waits for in-flight discovery.** client-go discovery calls take no context, so
  `Close` during a sync can wait for the running discovery requests, up to the 32s timeout.
- **A GET is authoritative in `GetManagedLiveObjs`.** A 404 on a GET removes the target from
  the result even when a copy was cached (for example in another API version). Upstream
  keeps the cached copy in that case.
- **`GetManagedLiveObjs` takes a `context.Context`.** This follows repository conventions;
  the method is otherwise as specified.
- **`ignoreResourceUpdates`** (DIFF-033 to 037), health (DIFF-066) and info enrichment
  (DIFF-089 to 100) belong to the caller's `PopulateResourceInfo` and update handler.
  Only the hooks are provided here.
- **Endpoints updates are not applied.** As upstream does, `Modified` watch events for core
  `Endpoints` are ignored (additions, deletions and relists are applied). Cached Endpoints
  can therefore show an old resourceVersion.

## Verification

Tests live in `internal/cache/*_test.go`:

- Unit and property tests: key round-trip, graph invariants and copy-on-write, hierarchy
  against a naive model, discovery selection, references, resource stripping, the paginated
  lister, the watcher state machine (410 relist, bookmarks, reconnect, 401/403, 404,
  periodic relist, backoff), processor ordering and batching, OpenAPI parsing, error
  classification, and unreachable clusters (closed local port, local 503 `httptest` server).
- Hermetic fake-cluster tests (`fakeapi_test.go`: an `httptest` server for discovery and
  GETs, and injected `ListerWatcher`s): an API that returns 404 while discovery still serves
  it loses its objects and converges once discovery drops it; a stopped registration whose
  relist fails leaves nothing behind; a transient discovery failure of a group (single
  version, or preferred version only) keeps its watches, objects and discovery data;
  `GetManagedLiveObjs` uses GET for list-denied scopes and dead registrations (object, 403
  error, 404 absent); `EnsureSynced` outlives callers with short deadlines and runs one sync
  for all of them, ignores the result of a sync for an older epoch, and releases waiters on
  `Close`.
- envtest integration tests (skipped unless `KUBEBUILDER_ASSETS` is set): initial sync;
  create, update and delete events; hierarchy across namespaced and cluster-scoped owners
  and inferred relations; CRD registration and removal at runtime; exclusions;
  namespace-restricted mode; relist after an injected 410 (wrapped `ListerWatcher`);
  managed versus unmanaged retention; the `GetManagedLiveObjs` GET fallback and version
  conversion; RespectRBAC none/normal/strict with an RBAC-restricted envtest user;
  invalidation, recovery and background resync; OpenAPI; the Manager; and a concurrency
  stress test run under `-race`.
