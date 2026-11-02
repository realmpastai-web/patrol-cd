# Component: `internal/controller` (Application and Operation reconcilers)

Status: implemented (reconcilers, retention and the `Run` wiring helper; the `patrolcd
controller` command, the tree API server and sharding are wired by their owners). Decision
records: ADR-0011 (Operation resource and sync state machine), ADR-0003 (resource model),
ADR-0004 (no Redis), ADR-0008 (sharding). Normative behavior:
`docs/architecture/sync-state-machine.md`. Parity areas: `docs/parity/areas/01-application.md`
(lifecycle) and `docs/parity/areas/05-synchronization.md` (automated sync, retries, history,
rollback, termination). Status overlay: `docs/parity/status/controller.yaml`.

Behavior was researched in Argo CD v3.5.5 `controller/appcontroller.go`, `controller/sync.go`
and `controller/hook.go` and re-implemented on controller-runtime; no upstream code is copied.

## Packages

| Package | Responsibility |
|---------|----------------|
| `internal/controller` | `Run(ctx, RunConfig)`: builds the manager (cache scoped to the control-plane namespace, or cluster-wide for Applications/Operations when `application.namespaces` is set), the settings manager, cluster registry, repository resolver, live-state caches, comparison manager, server-side diff, sync engines, both reconcilers, field indexes and the settings/cluster subscriptions, then runs until `ctx` ends. |
| `internal/controller/core` | Shared dependencies (`Deps`), parameters (`Params`, `ParamsFrom`), `NamespaceMatcher`, Operation listing/ordering/index (`OperationApplicationIndex`, `ListOperationsLive`, `ApplicationLabelValue`), optimistic-concurrency writes (`PatchStatus`/`PatchObject` re-read and re-apply a mutate derived from the object; `PatchStatusStrict`/`PatchObjectStrict` fail with `ErrConflict`; `ErrGone`), history (`AppendHistory`, `RevisionHistoryLimit`), refresh levels (`CompareLevel`, `RefreshRequester`), Event reasons, Prometheus `Metrics`, `SkipReconcile`, `LookupClusterResolver`. |
| `internal/controller/app` | Application reconciler: refresh decisions and levels, comparison, status, conditions and Events, automated sync and self-heal, deletion (finalizers, cascade, delete hooks), live-state event mapping, in-memory comparison state (`State`). |
| `internal/controller/operation` | Operation reconciler: admission, revision resolution and plan persistence, one sync step per reconcile, retries, termination, sync timeout, completion side effects; `KubeEngines` (sync engines per destination, impersonation), `NextRetryAt`, `MergeSyncOptions`, `Summary`. |
| `internal/controller/retention` | `Select`/`Collect`/`CollectIfNeeded`/`Needed`: keep the newest `revisionHistoryLimit` terminal Operations per Application; the API server is listed only when the cache shows more than the limit. |
| `internal/controller/clusterinfo` | Cluster-info updater: `New(Options)`, `(*Updater).Run`, `Sync`. Writes `Cluster.status` of the clusters of owned shards from the live-state caches (below). |
| `internal/controller/integration` | envtest integration tests only. |

## Public API

| Symbol | Purpose |
|--------|---------|
| `controller.Run(ctx, RunConfig{RestConfig, Namespace, Params, RepoServer, Registerer, Logger, LeaderElection, LeaderElectionID, Engine, CacheBase, PersistResourceHealth, ServerSideDiff, RepoErrorGracePeriod, JQTimeout, ClusterOptions})` | Command wiring. Metrics go to `Registerer`; the caller serves them and the probes. |
| `core.Deps{Client, APIReader, Recorder, Comparer, Settings, LiveState, ClusterConfig, Clusters, Namespace, Params, Metrics, Refresher, OwnsCluster, Logger, Clock}` | Dependencies of both reconcilers. `OwnsCluster` receives `nil` for an unresolvable destination (shard 0); `sharding.Ownership.OwnsClusterObject` fits it. |
| `core.Params{ApplicationNamespaces, StatusProcessors, OperationProcessors, SelfHealTimeout, SelfHealBackoff, SyncTimeout}`, `core.ParamsFrom(*settings.Params)`, `core.DefaultParams()` | `patrol-cmd-params-cm` controller keys (`controller.self.heal.*`, `controller.sync.timeout.seconds`, `controller.*.processors`, `application.namespaces`). Refresh intervals are read from `patrol-cm` (`timeout.reconciliation[.jitter]`, `timeout.hard.reconciliation`) on every reconcile. |
| `app.New(app.Config{Deps, DeletionKube, Jitter, DeletionPoll})`, `(*Reconciler).SetupWithManager`, `app.IndexFields`, `OnClusterCacheCreated` (pass to `cache.ManagerOptions.OnCacheCreated`), `Refresher()`, `RefreshAll(ctx)` (settings changes), `State(key) (AppState{Tree, Managed, ComparedAt}, bool)`, `AutomatedOperationName` | Application reconciler. |
| `operation.New(operation.Config{Deps, Engines, PollInterval})`, `SetupWithManager`, `NewKubeEngines(KubeEnginesOptions{Config, Engine, Metrics, TTL, Logger, Clock})` (`Engine`, `Forget`), `HealthFunc`, `EffectiveRetry`, `RetryAllowed`, `NextRetryAt`, `ParseRetryDuration`, `MergeSyncOptions`, `Summary` | Operation reconciler. |
| `retention.Select(ops, limit, keep)`, `retention.Collect(ctx, c, reader, app, log)`, `retention.CollectIfNeeded(ctx, c, reader, app, current, log)`, `retention.Needed(ops, limit)` | Operation retention. |
| `app.DeletionKube{Create, Get, Delete}` (`*sync.KubeClient`), `app.DeletionKubeFunc` | Delete-hook creation and cascade deletion client per destination (impersonated). |

## Application reconciler

Per reconcile (one Application, cache read):

1. Ignored: namespace not allowed (`application.namespaces`, APP-067), `skip-reconcile` on the
   Application or its destination Cluster (APP-088, REPO-097), destination owned by another
   shard (`OwnsCluster`).
2. Deleting → finalizers (below).
3. Refresh decision (`needRefresh`, upstream `needRefreshAppStatus`), in order: refresh
   annotation (force-resolve; `hard` also bypasses the manifest cache); spec sources differ from
   `status.sync.comparedTo` (force-resolve); hard/soft expiry of `reconciledAt` (latest); spec
   destination or ignoreDifferences differ from `comparedTo`; `metadata.generation` not yet
   observed (latest); an operation completed after `reconciledAt` (latest for automated,
   force-resolve for manual); a controller request (live-state change → recent, post-operation,
   dependency changes and self-heal backoff → latest). Without a reason only the next periodic
   refresh is scheduled (`reconciledAt + timeout + rand[0, jitter)`, or the hard timeout).
4. Before comparing, the Application is read again from the API server and the decision is
   repeated, so a cache that lags behind the controller's own status write neither repeats a
   comparison nor feeds a stale automated-sync decision.
5. Project resolution and `project.ValidateApplication`: issues become `InvalidSpecError` /
   `UnknownError` (status True) with sync and health `Unknown` and no comparison (APP-033/036).
   The stored state becomes an empty tree with no managed resources, so the tree API serves an
   empty tree and answers managed resources as unavailable, which the API reports as the
   Application's conditions (412), as upstream's empty tree and cache miss do. A tree-only
   refresh then compares again instead of rebuilding the tree from the last valid comparison.
6. `compare.Compare` at the level: recent compares at `status.sync.revision(s)`, latest and
   force-resolve at the spec target revisions (force bypasses the revision cache).
   `ErrRepoErrorDeferred` keeps the previous status (retry in 30s). The last Operation's
   `syncResult` (managed namespace metadata) and local manifests are passed on.
7. Pre/post-delete finalizers (and their `/cleanup` stage) are added or removed from the
   rendered hooks before the status write (APP-064/065).
8. Automated sync (below) when sync windows allow an automated sync now; otherwise `SyncError`
   is left untouched.
9. Status write (status subresource, JSON merge patch with a `resourceVersion` precondition,
   re-read and re-applied on conflicts, skipped when nothing changed): `sync`, `health`
   (+lastTransitionTime on change), `resources`, `resourceHealthSource`, `sourceType(s)`,
   `summary` (when the tree was built), `reconciledAt` (only for latest/force-resolve or when
   unset, APP-042), `controllerNamespace`, `observedGeneration`, conditions. A status the API
   server rejects as too large is replaced by the previous one plus an `UnknownError` with the
   upstream message (APP-087). The refresh annotation is removed afterwards.
10. Events: `ResourceUpdated` "Updated sync status: A -> B" / "Updated health status: A -> B".

Conditions are `metav1.Condition` with `observedGeneration`. Each producer is authoritative for
its types (`compare.MergeConditions`): spec validation for `InvalidSpecError`/`UnknownError`,
the comparison for `ComparisonError`, `SharedResourceWarning`, `RepeatedResourceWarning`,
`ExcludedResourceWarning`, `ResourcesTruncatedWarning`, the tree for `OrphanedResourceWarning`,
automated sync for `SyncError`, deletion for `DeletionError`. Aggregates: `Reconciled`
(ReconcileSucceeded/ReconcileFailed), `Synced` (Synced/OutOfSync/SyncStatusUnknown),
`Healthy` (reason = health code).

Live-state events (`OnResourceUpdated` of every cluster cache, registered with the cache key
by `OnClusterCacheCreated`), in upstream order: an update (never a create or a delete) hidden by
the `ignoreResourceUpdates` rules (`compare.SkipResourceUpdate`: equal update hashes and equal
health) is dropped first, before attribution, so the rules also cover untracked descendants
annotated with `cd.patrol.example/ignore-resource-updates` (APP-110). The Applications of the
old and of the new object are then collected (tracking of a root, else the owner chain,
`compare.AppOf`), so an object moving between Applications refreshes both; core Endpoints
count for none (DIFF-037). The Application whose root object (tracked, no owners) changed gets
a recent comparison; one whose descendant changed gets a tree-only refresh (tree rebuilt from
the managed resources of the last comparison, `summary` and `OrphanedResourceWarning`
persisted; falls back to a recent comparison after a restart). The changed Application object
itself (same UID: an Application of that name on a destination cluster is another object),
Applications outside the allowed namespaces and tracking values no Application can have
(tracking metadata can hold any string) are skipped. A namespaced object attributed to no
Application gives a tree-only refresh to the Applications that monitor
orphaned resources in its namespace on the same cluster (APP-085, DIFF-078; an object tracked
by an Application that does not exist counts as an orphan in a tree build, as upstream, but its
event is attributed and refreshes no monitor): two in-memory
field indexes (AppProjects with `spec.orphanedResources`, Applications by destination
namespace) find them without API calls, and Applications of other shards are skipped. The
handler runs on the cache's event goroutine, so it reads only the informer cache and the
cluster registry. Each refresh request is logged (`requesting app refresh caused by object
update`, object identity only): at info for a root object's change, which requests a
comparison, and at debug for descendant and orphan changes (OPS-061; upstream logs orphan-monitor
refreshes at info, since it compares those Applications).

Watches: Applications; Operations (mapped by `spec.application`); AppProjects, Clusters and
Repositories (Applications found through field indexes on project, destination server/name
and normalized repository URLs get a latest refresh); the channel of controller requests.
`RefreshAll` is called on settings changes by `Run`.

Controller requests (the refresh queue) coalesce per Application to the highest level and the
earliest due time. Notifications are bounded: an Application has at most one event in the
channel until it is reconciled, at most one timer for its earliest delayed request, and
Applications that do not fit in a full channel (4096) wait in an overflow set drained by one
queue timer. Requests made while nobody drains the channel (a standby replica under leader
election, a burst) cost one map entry per Application, not timers or goroutines.

The latest comparison (tree and managed resources with Secret data masked by
`Result.MaskSecrets`) is kept in memory per Application (`State`) for the tree API; it is
rebuilt by the next comparison after a restart and dropped when the Application disappears.

### Automated sync (SYNC-017..025)

`decideAutoSync` reproduces upstream `autoSync`: enabled (`automated` set, `enabled` not false);
no non-terminal Operation; not deleting; `OutOfSync`; without `automated.prune`, skip when every
non-Synced resource only requires pruning; "already attempted" = the summarized last Operation
targeted the same revisions (only sources are compared when `RevisionsMayHaveChanges` is false)
and the same sources (a completed Operation without sync result counts as attempted). A failed
attempt yields `SyncError` "Failed last sync attempt to [revs]: <message>"; a successful one
without self-heal is skipped; with self-heal the backoff (`wait.Backoff` steps:
2s, 6s, 18s, …, capped; or the fixed `controller.self.heal.timeout.seconds`) minus the time
since the last operation finished must have elapsed, else a latest refresh is requested for
the remaining time; the self-heal Operation lists the non-Synced resources (partial sync, no
hooks, no history) and increments `autoHealAttemptsCount`. With prune and without
`allowEmpty`, an Operation that would prune every resource is refused with `SyncError`
"Skipping sync attempt to [revs]: auto-sync will wipe out all resources".

The Operation is created with `initiatedBy.automated=true`, `sync.revision(s)` from the
comparison, `prune`, the Application's sync options, `retry` = `syncPolicy.retry` or
`{limit: 5}`, the `cd.patrol.example/application` label and a controller owner reference.
Its name `<app>-auto-<10 hex>` hashes the Application UID, the revisions, the self-heal attempt,
the spec sources and the Operation summarized in `status.operationState` (name and start):
concurrent evaluations of the same state get `AlreadyExists` and do nothing (also when the
existing Operation already ran, i.e. this evaluation has not observed its completion yet).
Once any other Operation ran (a manual sync, an earlier heal), the summary differs and the
next decision gets a new name, so automated sync and self-heal are never blocked by an older
Operation of the same revisions and attempt (upstream always issues the operation). A name
taken by an Operation that never ran (Rejected, or cancelled while Pending) is retried with
"-1".."-4" and then a generated name. Event `OperationStarted` "Initiated automated sync to
'<revs>'".

### Deletion (APP-057..066, SYNC-070)

With any controller finalizer present and no Running/Terminating Operation (Pending ones are
rejected), one stage per pass, in upstream order:

1. Unresolvable destination: every deletion finalizer is removed in one patch, nothing is
   touched (APP-063).
2. `pre-delete-finalizer`: PreDelete hooks rendered at the spec target revisions are created
   (destination namespace when namespaced, scope from discovery, project permission checked
   per hook, tracking applied); a created hook waits for the live-state cache. On
   `AlreadyExists` the object is read: carrying this Application's tracking (an earlier pass's
   hook the cache has not observed, or a kind the cache excludes) it is assessed from that
   read; otherwise the hook is skipped with a Warning Event `DeleteHookSkipped` (upstream skips
   it with a log line) and deletion proceeds without it. Progressing hooks wait; Degraded ones
   are deleted and the pass fails with "PreDelete hook(s) failed: ns/name, …"; all Healthy (or
   without health) removes the finalizer.
3. Propagation finalizers: the managed live objects the project permits, excluding CRDs, the
   Application itself, `Delete=false` (resource annotation over Application option) and
   `helm.sh/resource-policy: keep`; any object already terminating → wait; an object requiring
   confirmation (`Delete=confirm`/`Prune=confirm`) without a `deletion-approved` timestamp at
   or after the deletion timestamp → wait; otherwise only the highest sync wave is deleted
   (Background for the `/background` variant first in list order, Foreground otherwise). No
   deletable object left → propagation finalizers removed.
4. `post-delete-finalizer`: PostDelete hooks, same rules as 2.
5. `/cleanup` stages (pre, then post): hooks without a delete policy, or whose policy matches
   the aggregated hook health (`HookSucceeded`/Healthy, `HookFailed`/Degraded), are deleted;
   the finalizer is removed once they are gone.

Errors set `DeletionError` and record a Warning Event `StatusRefreshed` "Unable to delete
application resources: …"; the condition stays until a stage completes. Deletes and hook
creation use the impersonated identity when impersonation is enabled.

## Operation reconciler

One step per reconcile; every outcome is persisted on the Operation (status subresource,
optimistic concurrency) before returning, and mirrored into `Application.status.operationState`
(only for admitted Operations: the active one takes over, a terminal one only replaces a
summary of an Operation that started earlier).

Operation writes (status, labels, owner reference) are strict: a step's values are computed
from the Operation it read, so a 409 abandons the step (`core.ErrConflict`, requeue after
`PollInterval`, normally preceded by the newer object's watch event) instead of re-applying
them over a newer status written by another replica or after `spec.cancel`; a terminal phase
is never changed. The Application summary and history writes are derived from the re-read
Application and keep the re-read/re-apply loop. The Operation labels are ensured before
admission: `cd.patrol.example/application` is added, or corrected when it names another
Application, because admission and retention list Operations from the API server by that
label (plus the unlabelled ones, both filtered by `spec.application`).

| Phase | Step |
|-------|------|
| `""`/`Pending` | Labels (`application` when a valid label value, `operation-phase`) and the controller owner reference are ensured. `spec.cancel` → `Cancelled`. No sync/rollback → `Rejected`/InvalidSpec. Application deleting → `Rejected`/ApplicationDeleting. Another non-terminal Operation of the Application (listed from the API server) that is Running/Terminating, or Pending and older (creationTimestamp, then name) → `Rejected`/OperationInProgress "another operation is already in progress". Rollback with automated sync enabled → `Rejected`/RollbackNotAllowed "rollback cannot be initiated when auto-sync is enabled"; unknown id → `Rejected`/RollbackTargetNotFound "application <ns/name> does not have deployment with id <id>"; entry without source → `Rejected`/RollbackNotAllowed. Sync windows (manual = not automated): an automated Operation is deleted; a manual one stays `Pending` with `Admitted=False`/SyncWindowClosed "Sync operation blocked by sync window" until the next window transition. Otherwise `Running`, `startedAt`, `Admitted=True`. |
| `Running` | `spec.cancel` → `Terminating` "operation is terminating"; `startedAt + controller.sync.timeout.seconds` passed → `Terminating` "operation is terminating due to timeout". `finishedAt` set = waiting for a retry: before `NextRetryAt` requeue, then "Retrying operation[ with latest revisions]. Attempt #n" with results/progress reset (and plan reset with `retry.refresh`). Project failure → attempt Error "Failed to load application project: …". Sync windows block → message "Sync operation blocked by sync window", stays Running (SYNC-087). Spec issues → Error "InvalidSpecError: …". Overridden sources (`sync.source(s)`, a rollback's history entry) are validated like the Application's (`project.ValidatePermissions`: source fields, `sourceRepos`, destination) → Error/InvalidSpec "InvalidSpecError: application repo <url> is not permitted in project '<p>'". First step (no plan): compare at the requested revisions (rollback entry, `sync.revision(s)`/`source(s)` override, or spec; the revision cache is bypassed) → `status.plan` (resolved revisions, rendered-input digest, resolved destination, warnings), `syncResult.revision(s)/source(s)`, `progress.totalTasks`, `Planned=True`. Later steps: compare at the persisted revisions, build `sync.Input` (tracking, shared-resource and self-reference callbacks from `internal/tracking`, per-task project permission, discovery scope from the cluster cache, `RespectIgnoreDifferences` normalizer, operation sync options merged over the Application's), `Engine.Step`, persist results/progress/message; requeue after `RequeueAfter`. Comparison error conditions → Error "ComparisonError: …". Transient engine errors keep the partial result and requeue without consuming retries. A changed rendered-input digest adds a plan warning. |
| attempt `Failed`/`Error` | While `retry.limit` allows (`< 0` unlimited, `0` none): stays `Running`, `finishedAt=now`, `retryCount++`, "<msg>. Retrying attempt #n at <3:04PM>.", `Retrying=True`. Exhausted: terminal with " (retried n times).". An unparsable backoff → Error "<msg> (failed to retry: unable to parse … as a duration)". |
| `Terminating` | With a persisted plan, `Engine.Terminate` cleans up running hooks; the Operation ends `Cancelled` "Operation terminated" (or `Error` "Operation termination had errors: …"), plus ", triggered by controller sync timeout" and the retry suffix. No retry. A destination, impersonation ("failed to find a matching service account to impersonate: …") or engine error ends `Error` without building an engine, so hooks are never cleaned up with the controller's identity when impersonation is enforced (upstream returns before `Terminate`). |
| terminal | Completion side effects: `finishedAt`, `Completed=True` (reason by outcome), Application summary, history entry for successful, non-dry-run, non-partial syncs (`id` = last + 1, `deployStartedAt`, `deployedAt`, `initiatedBy`, revision(s)/source(s), `operationName`; truncated to `revisionHistoryLimit`; recorded once per Operation name), Event `OperationCompleted` "[Partial ]Sync|Rollback operation to <rev> succeeded|failed: <msg>", metrics, a refresh request (latest for automated, force-resolve for manual, none for dry runs), retention. A later reconcile of a terminal Operation repairs a summary/history write lost in a crash and re-checks retention. Retention counts the Application's terminal Operations in the cache (the completing one with its new phase) and lists the API server only above the limit, so reconciling every retained Operation at start is O(retained) in-memory work. |

Impersonation: with `application.sync.impersonation.enabled`, `project.SelectServiceAccount`
picks the destination service account; the engine's REST config impersonates it
(`clusters.ImpersonatingConfig`). Enforced and unmatched → Error "failed to find a matching
service account to impersonate: …". Engines are cached per (server, user, settings
fingerprint, Cluster UID and generation) for 10 minutes (bounded to 1024), dropped by `Forget` on
cluster credential or spec changes. The key has no resourceVersion: the cluster-info updater
writes the status on its own schedule, and a REST config is not built from it.

Restart recovery: everything needed to continue is in `Operation.status` (plan with resolved
revisions, per-task results, progress, retry state). A new controller re-plans from the
persisted revisions and the engine adopts its hooks (deterministic names) and treats persisted
completed tasks as done; no in-memory state is required.

## Cluster-info updater

`clusterinfo.Updater` runs in every replica, from `RunController`. A round takes the clusters of the
assigner, keeps those of owned shards and writes what `ComputeClusterInfo` gives for each. A cluster
therefore has one writer; the synthetic in-cluster cluster has no object and gets no status.

- **Interval:** `PATROL_UPDATE_CLUSTER_INFO_TIMEOUT`, a Go duration from 10 s to 1 min, default
  10 s, read by `RunController` unless `ControllerConfig.ClusterInfoInterval` is set; the updater
  logs it when it starts. The interval also bounds a round: the next one starts where it stopped,
  so that the first clusters of a long list do not use every round.
- **Reads:** the manager's cached client and `cache.Manager.Lookup`. A round never creates a
  live-state cache and contacts no destination, so it cannot hang on one.
- **Writes:** the status lives in etcd (ADR-0004), where every write reaches every Cluster
  informer. An unchanged status is not written. A change of the cache statistics alone waits for
  `CacheInfoWriteInterval` (1 min). Anything else is written at once. `attemptedAt` moves when the
  connection status or message changes: it says since when that state holds, not when it was last
  checked. The implicit in-cluster cluster has no object and the registry stamps its state on every
  read, so the UI shows no such time for it.
- **Reactions:** status writes reach no comparison and no sync engine. The Application reconciler's
  Cluster watch passes a new UID and changes of generation, labels and annotations only, as the
  ApplicationSet handler does, and the cluster registry sends no event for them.
- **Application count:** Applications of the namespaces this controller serves whose project
  exists and permits their namespace. A failed list ends the round without writing, because a count
  of zero would be a guess.
- **Skip-reconcile:** no shard processes such a cluster, so it reports `Unknown` whatever an old
  cache says.
- **Version and API versions:** validation, `GET .../manifests` and the CLI's local render read them
  from the status, where upstream asks the destination (`GET .../manifests` asks it only if the status
  lacks one). So a status with nothing newer keeps them:
  a cluster with no Applications, a skip-reconcile cluster, the rounds after a controller restart
  and a cache that has not reported report `Unknown` or `Failed` with the last version and list
  known. Only a destination's answer replaces them, and `spec.server` cannot change, so what stays
  is this destination's. Connection state and cache statistics are not kept.
- **Invalidated cache:** `Unknown` until the next sync, as upstream, because the time and the
  outcome of the old sync say nothing about what the cache holds now (`compare.ClusterCacheState`).
- **Resource version:** a status write moves it, so a client that sends the version it read can
  get a 409 with no change to what it edits. The UI's cluster form sends none: `updatedFields`
  confines its write.

A destination that fails under a synced cache keeps `Successful` until the next full sync of that
cache (restart, invalidate-cache, credential change, 12 h resync). Upstream behaves the same way:
the sync error is set by full syncs only. The Applications report the failure earlier.

## Metrics

`patrol_app_sync_total{namespace,name,project,dest_server,phase,dry_run}`,
`patrol_app_sync_duration_seconds_total{namespace,name,project,dest_server}`,
`patrol_app_reconcile_duration_seconds{namespace,dest_server}`,
`patrol_app_operation_step_duration_seconds{phase}`,
`patrol_app_k8s_request_total{server,verb,response_code}` (every request of the engines,
deletion clients and live-state caches built by `Run`), `patrol_app_auto_sync_total{outcome}`,
`patrol_app_deletion_passes_total{outcome}`,
`patrol_app_orphaned_resources_count{namespace,name,project}` (set after every tree build; only
for Applications whose project monitors orphaned resources; removed when the Application is
deleted).

## Failure modes

| Situation | Result |
|-----------|--------|
| Settings not loaded | reconcile error (rate-limited retry) |
| Write conflict on an Application | re-read from the API server and re-apply (mutates derived from the object), 5 times, then a rate-limited retry |
| Write conflict on an Operation | the step is abandoned and recomputed from the newer object (requeue after `PollInterval`) |
| Object deleted during a write | `ErrGone`, reconcile ends without error |
| Cache behind the controller's write | Application re-read before comparing; admission and retention list from the API server |
| Repo error within the grace period | previous status kept, retry in 30s |
| Destination unreachable | `ComparisonError` (comparison); Operation step requeued while the cache is unavailable |
| Transient API errors during a sync step | partial result persisted, requeue after `PollInterval`, no retry consumed |
| Controller killed mid-operation | next controller resumes from `Operation.status` (tested) |
| PreDelete/PostDelete hook fails | hook deleted, `DeletionError`, re-created on the next pass |

## Security notes

- Manifests, Secret data and credentials are never logged; logs carry names, revisions,
  phases and API error text. Stored managed resources are masked (`MaskSecrets`).
- Every sync task, delete hook and cascade candidate is checked against the effective project
  (destination and kind restrictions) in the controller, independently of the API server.
  Operation source overrides and rollback sources are checked against `spec.sourceRepos`
  the same way. The `override` permission upstream requires for local manifests
  (`spec.sync.manifests`) and source overrides cannot be evaluated by the controller for
  Operations created directly with kubectl; until the Operation admission policy gates those
  fields (requested below), granting `create` on Operations implies that privilege (the
  destination and kind restrictions still apply).
- Automated Operations carry `initiatedBy.automated=true`, which the Operation admission
  policy only accepts from identities with the `impersonate` verb on operations (the
  controller's).
- Impersonation enforcement fails closed. Status messages are bounded (32 KiB on Operations,
  4 KiB in the Application summary).

## Parity IDs

`docs/parity/status/controller.yaml`: APP-002, 032..037, 042, 044, 046, 057..060, 063, 066,
067, 078..088 (086, 087 in-progress), 110, 141 (in-progress); DIFF-036, 037; OPS-073;
SYNC-001, 003, 006, 013..028, 031 (deletion confirmation), 051, 052, 070, 076..079, 087,
089..091, 093, 094; TEN-010; REPO-094 (in-progress), 095; OPS-050; REPO-097 (in-progress); APP-045
different-by-design.

## Known gaps and deliberate differences

- **Operation objects** replace `Application.operation` (ADR-0003): admission is asynchronous
  (`Rejected` instead of a 409 when the API server's pre-check is bypassed), and an Operation
  already Running always keeps the slot even when an older Pending one appears later (second
  precision creation timestamps).
- **Termination** ends `Cancelled` (state machine) instead of upstream `Failed`.
- **Sync windows at admission**: a manual Operation waits `Pending` (`SyncWindowClosed`);
  automated Operations are deleted. Running Operations pause as upstream.
- **Retry timing**: the wait before attempt n is `duration × factor^n` from the failure, as
  upstream effectively waits; the message shows that time (upstream's message shows the
  `factor^(n-1)` time while waiting `factor^n`).
- **History** is recorded once per Operation (keyed by name) and also for rollbacks; dry runs,
  selective and self-heal syncs record none.
- **Sync options**: the operation's options are merged over the Application's
  (`OperationSpec.sync.syncOptions` contract), so Operations created with kubectl inherit them.
- **Refresh after a completed operation** is also derived from state (`finishedAt` after
  `reconciledAt`) so it survives restarts; it is also triggered for dry runs.
- **Orphaned-resource refreshes**: an unattributed object refreshes the orphan-monitoring
  Applications with a tree-only refresh (upstream: `CompareWithRecent`); the orphaned nodes,
  `OrphanedResourceWarning` and gauge are the same. A managed resource that was Missing at the
  last comparison, and a sync status affected by such an object, wait for the next comparison.
  Monitors are matched by namespace and by the event's cluster (upstream: namespace only).
- **`patrol_app_orphaned_resources_count`** has one series per Application, under its current
  project (upstream keeps an old project's series). It is not emitted for Applications whose
  project does not monitor orphaned resources (upstream sets 0), and is removed when the
  Application is deleted, has an invalid spec, or is no longer processed by this replica
  (namespace no longer allowed, cluster of another shard).
- **Repo error grace** handling is implemented but not exercised by controller tests (the
  harness runs with no grace period).
- **Status size fallback** (APP-087) is implemented but not tested (envtest does not reach
  etcd's limit cheaply).
- **First observation after a restart** does not force a comparison; the persisted
  `reconciledAt` decides as upstream.
- **Tree API**: `State` exposes the tree and masked managed resources; the `treeapi.StateProvider`
  adapter (server-side diff with the Application's diff settings needs a `compare` API that
  does not exist yet) is left to the tree API wiring.
- **Delete hooks of an excluded kind** are created and awaited (health read from the object),
  but the `/cleanup` stage only sees hooks in the live-state cache, so they are left behind.
- **Implicit in-cluster cluster:** no object, so the updater skips it. A default install shows no
  Application count, API versions or cache information for its main destination, the CLI reads 0
  there, and the UI "Not reported". `GET manifests` asks the cluster itself. Upstream walks the
  local cluster too. Registering a Cluster object for that server is the workaround
  (#246).
- **Cluster information does not expire.** Upstream's Redis entry lapses after 10 minutes. Here
  the status stays at its last value when every controller stops, and `attemptedAt` cannot tell a
  live updater from a dead one, because a steady status is not written.
- **Hydrator**, **sharding wiring** in `Run`, and the `patrolcd controller` command are not
  part of this package yet; `Deps.OwnsCluster` is the sharding hook.

## Requests to other owners

- `deploy/crds/operations-admission-policy.yaml` (CRD/deploy owner): on CREATE, require an
  `override` verb on `operations.cd.patrol.example` (authorizer check, as done for
  `impersonate`) when `spec.sync.manifests`, `spec.sync.source` or `spec.sync.sources` is set
  (and for `spec.sync.revision(s)` when `application.sync.requireOverridePrivilegeForRevisionSync`
  is on), granted to patrol-server, which checks Patrol RBAC `override` itself.
- `api/v1alpha1/conditions.go`: declare `ApplicationDeleting`, `RollbackNotAllowed` and
  `Terminating` Operation reasons (defined locally in `operation`), and `ResourcesTruncatedWarning`
  (already requested by `compare`).
- `internal/compare`: an exported server-side diff entry point with the Application's diff
  options (`Manager.ServerSideDiff(ctx, app, proj, live, target)`) so the controller can
  implement `treeapi.StateProvider.ServerSideDiff`.
- `docs/architecture/sync-state-machine.md`: record that a Running/Terminating Operation always
  keeps the slot, the retry wait/message semantics above, and that automated Operations
  blocked at admission are deleted (already stated) while Running ones pause.

## Verification

Run on 2026-10-16 (darwin/arm64, Go 1.27.2, envtest kube-apiserver 1.36.0), from the
repository root, after the review fixes below:

| Command | Result |
|---------|--------|
| `go build ./...` | exit 1, only in `internal/cli/app` (another workstream's package under construction); `go build ./internal/controller/...` exit 0, and every package outside `internal/cli/...` builds |
| `go vet ./internal/controller/...` | exit 0 |
| `golangci-lint run ./internal/controller ./internal/controller/{app,core,operation,retention,integration}/...` | `0 issues.` |
| `KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 go test -race -count=1 -v ./internal/controller/{app,core,operation,retention,integration}/...` | exit 0; `ok` for all five packages; 66 top-level tests passed (33 unit, 33 envtest), none failed or skipped |
| same with `-count=2` (flakiness check) | `ok` for all five packages (integration 60s) |
| `go run ./hack/parity -check` | exit 0 |

Review fixes (2026-10-16), each with a test that fails on the previous code (the six envtest
tests were run against a copy of the previous controller code: all six failed):

| Finding | Fix | Tests |
|---------|-----|-------|
| Automated Operation names collided after a manual sync (self-heal and auto-sync back to a revision silently stopped) | name also hashes the summarized Operation; generated-name fallback after suffixes | `TestSelfHealAfterManualSync`, `TestAutoSyncBackAfterManualSync`, `TestAutomatedOperationName`, `TestCreateAutomatedOperation` |
| Delete hook `AlreadyExists` waited forever for an untracked object or an excluded kind | read the object: tracked → assessed from the read; untracked → skipped with `DeleteHookSkipped` | `TestPreDeleteHookNameTaken`, `TestPreDeleteHookExcludedKind` |
| Termination ignored the impersonation error and ran with the controller identity | termination fails with the impersonation error, no engine built | `TestTerminateWithoutMatchingServiceAccount` |
| Operation source overrides bypassed `sourceRepos` | overridden and rollback sources validated with `project.ValidatePermissions` | `TestOperationSourceOverrideNotPermitted` |
| Unfiltered API-server Operation lists on every terminal reconcile | label-selected lists; retention gated on the cached count | `TestListOperationsLive`, `TestCollectIfNeeded`, `TestNeeded` |
| Refresh queue started a timer chain per request when the channel was full | one outstanding event per key, overflow set with one timer, one timer per key for delayed requests | `TestRefreshQueueBoundsNotifications` |
| Operation conflict retries re-applied stale step values | strict Operation writes, step abandoned on 409, terminal phase guard | `TestStaleStepIsAbandoned`, `TestPatchStatusStrict` |

Earlier `-count=2` runs exposed four races that are fixed and covered by these runs: a
delete-hook pass completing because the live-state cache had not yet observed a hook it had
just created (`AlreadyExists` now reads the hook and assesses it), an automated sync skipped because the Operation
cache still showed a completed Operation as Running (confirmed through the API server), a
conflict retry marking a newer spec generation as observed (the compared generation is
written instead), and test assertions racing the completion write.

Unit tests: refresh decision table, expiry and jitter, refresh queue coalescing and bounded
notifications; automated sync decision table, automated Operation creation (dedupe, suffixes,
generated name), `alreadyAttemptedSync` (upstream `TestAlreadyAttemptSync` cases),
`selfHealRemainingBackoff` (upstream `TestSelfHealRemainingBackoff` table), automated
Operation names (property: always a valid DNS subdomain); deletion helpers (highest wave,
propagation policy, exclusions, explicit delete policies); retry backoff table and
monotonicity property; summary rules; option merging; history (property: bounded, consecutive
ids); retention (property: only the oldest terminal Operations are selected; API listing
only above the limit); strict writes and stale steps (conflict abandons the step, terminal
phases kept); label-selected Operation listing; namespace matcher; parameters; request
counting transport.

envtest integration tests (`internal/controller/integration`): real kube-apiserver as the
control plane and as the destination (in-cluster style REST config), real
`internal/cache`, `internal/compare`, `internal/diff`, `internal/sync`, `internal/tracking`,
`internal/project`, `internal/settings`; fake repo-server with commits per branch; a fake
garbage collector releases foreground deletions. Cases: create → OutOfSync → manual sync →
Synced with history, summary, conditions, Events, owner reference and in-memory state;
automated sync of new revisions with prune; prune-only changes without auto-prune; drift →
OutOfSync → self-heal (partial, no history), second self-heal after the backoff, no heal
without selfHeal, self-heal after a manual sync reset the attempt counter, automated sync back
to the target revision after a manual sync to another branch; failing sync with retries/backoff → Failed "(retried 2 times)." and SyncError
without a new attempt, then recovery on a fixed commit; retry.refresh picking up a new commit;
terminate with a running PreSync hook and the operation slot (`Rejected`); sync timeout;
rollback (history ids, unknown id, auto-sync enabled); history limit and Operation retention;
sync windows (Pending manual Operation, no automated Operation, admission after removal);
skip-reconcile; missing project; Applications in other namespaces (allowed, not permitted,
ignored); refresh annotation and recent refresh keeping `reconciledAt`; repo errors →
ComparisonError, Operation Error with the formatted condition, recovery; cascade deletion in
reverse waves with Delete=false and Helm keep; foreground and non-cascading deletion;
unresolvable destination; deletion confirmation with a stale and a valid approval;
PreDelete hook success and failure (DeletionError), hook name taken by an untracked object
(skipped), hook of a kind the cache excludes (awaited from a direct read); PostDelete hook and
cleanup; impersonation (forbidden without RBAC, allowed with a Role, enforced without a match,
termination without a match keeps the hooks); source override outside `sourceRepos` (Error)
and a permitted override (synced, recorded in `syncResult.source`); shard
filter; metrics; controller killed twice during a multi-wave sync (hook adopted, Deployment
not re-created, sync completes).
