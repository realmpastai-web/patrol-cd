# Component: cluster and project API (`internal/apiserver/services/{clusters,projects,links}`)

Status: implemented (handlers, Go client, tests); not yet wired into the patrol-server command.
Normative contract: [`api-contract.md`](../api-contract.md) ("Projects", "Clusters"). Framework:
[`apiserver.md`](apiserver.md). Libraries: [`clusters.md`](clusters.md) (`internal/clusters`),
[`project.md`](project.md) (`internal/project`), [`auth.md`](auth.md), [`rbac.md`](rbac.md),
[`settings.md`](settings.md). Upstream baseline: Argo CD v3.5.5 `server/cluster`,
`server/project`, `server/deeplinks`.

| Package | Contents |
|---------|----------|
| `internal/apiserver/services/clusters` | Cluster routes; `Connector` (connection checks with SSRF guardrails); `Sanitize` (API form of a Cluster) |
| `internal/apiserver/services/projects` | AppProject routes, project role tokens |
| `internal/apiserver/services/links` | Shared deep-link evaluator (`project.links`, and `application.links` / `resource.links` for the application API) |
| `pkg/apiclient/clusters.go`, `projects.go`, `links.go` | Request/response types shared with the server, client methods |

Both services export `New(Deps) *Service` (an `apiserver.Service`) and are listed in
`apiserver.DocumentedServices()`. With zero `Deps` they register their routes (OpenAPI) and answer
503.

## Wiring

```go
clusters.New(clusters.Deps{
    Config:    clusters.Config{Namespace: ns},
    Client:    ctrlClient,                       // Clusters, Secrets, AppProjects (namespace)
    Settings:  settingsManager,                  // cluster.inClusterEnabled
    Connector: clusters.NewConnector(clusters.ConnectorOptions{Options: clusterlib.Options{Tuning: ...}}),
    Status:    clusterlib.NewStatusWriter(dyn),  // records connection checks
    InCluster: registry,                         // status of the implicit in-cluster cluster
})
projects.New(projects.Deps{
    Config: projects.Config{Namespace: ns}, Client: ctrlClient, Settings: settingsManager,
    Tokens: tokenService, Events: kube.CoreV1().Events(ns), Policies: enforcer,
    Clusters: registry, // optional destination lookup for the update impact check
})
```

`Policies` pushes role policies to the RBAC enforcer as soon as this API changes a project; the
server must still follow AppProject watch events (kubectl, other replicas). The token service
must use `apiserver.ProjectTokenRegistry` so that removing a record revokes the token.

## Routes and RBAC

All routes need authentication (anonymous only with `users.anonymous.enabled`).

### Clusters (resource `clusters`)

The RBAC object is `<project>/<server>` for a project-scoped cluster, else the server URL
(`apiclient.ClusterRBACObject`, upstream `CreateClusterRBACObject`). `{server}` is the URL-encoded
server, or the name with `?id.type=name` (`name_escaped`: the name URL-encoded once more).

| Method | Path | Check | Behavior |
|--------|------|-------|----------|
| GET | `/clusters` | `get` per item | Effective clusters sorted by server (per server the object with the smallest name), plus the implicit `in-cluster` cluster unless `cluster.inClusterEnabled` is `"false"`. Filters `id.type`/`id.value`, `name`, `server` (each keeps the first match, upstream), `selector`, `limit`/`continue`. |
| POST | `/clusters?upsert=` | `create` on the request's object (+ `create` on `<server>` for `awsAuthConfig`/`execProviderConfig`) | Validate → `ValidateRegistration` → ambient-auth gate (below) → project exists → connection check (`error getting server version: …`) → create the Cluster and its credentials Secret → record the check in status. Identical existing cluster: returned. Different: `existing cluster spec is different; use upsert flag to force update; difference in keys "…"`, or with `upsert` a full update (needs `update` on the existing and on the new object). An existing server the caller may not get: 409 `cluster "<server>" already exists`. The implicit in-cluster cluster counts as existing: a different request materializes it through the update path with or without `upsert`, so it needs `update` on `https://kubernetes.default.svc` (unscoped); an identical request returns it without creating an object. |
| GET | `/clusters/{server}` | `get` | Missing and forbidden: the same 403 `permission denied`. |
| PUT | `/clusters/{server}?updatedFields=` | `update` on the existing object, and on the new project's object when `updatedFields` is empty or contains `project`; `update` on `<server>` when the written config sets `awsAuthConfig`/`execProviderConfig` | `updatedFields` (repeated or comma-separated: `name`, `namespaces`, `config`, `shard`, `clusterResources`, `labels`, `annotations`, `project`; unknown ignored) copies only those paths; without it the cluster, including its credentials, is replaced. The paths are applied to the latest stored object on every write attempt (after the connection check, and again on conflict retries), so fields outside `updatedFields` keep concurrent changes (rename, refresh annotation, labels). The stored object must still be the authorized one (same UID and server, else 409); if it moved to another project meanwhile, `update` is required there too. The server is immutable. Connection checked with the result (recorded only if the written connection settings are the checked ones). `resourceVersion` in the body: 409 when stale. The controller's status writes move it too, so a client that keeps it across more than a short read-modify-write gets 409 with no change to what it edits; `updatedFields` confines a write without it, which is what the UI does. Updating the implicit in-cluster cluster creates its object. |
| DELETE | `/clusters/{server}` | `delete` on each cluster | By name, every cluster with the name; all are authorized before any is deleted. API-managed credential Secrets are deleted (they are also owned by the Cluster); others are kept. The implicit in-cluster cluster: 404 `cluster "https://kubernetes.default.svc" not found`. Clusters used by Applications are deleted (upstream; their Applications go Unknown). |
| POST | `/clusters/{server}/rotate-auth` | `update` on each cluster | Legacy service-account bearer token required (`Cluster '<s>' does not use bearer token authentication`; other tokens: `cluster '<s>' does not use a legacy service-account token: …`). `internal/clusters.RotateManagerToken` against the destination; the new token (client certificate cleared) is probed, stored and recorded before the old token Secret is deleted. |
| POST | `/clusters/{server}/invalidate-cache` | `update` | Sets `cd.patrol.example/refresh` to now (RFC 3339) with a merge patch; returns the cluster. |

Request body (`apiclient.ClusterInput`, Argo CD's `Cluster` message): `server`, `name`
(default: the server), `namespaces` (trimmed, empty dropped, DNS-1123 labels, no duplicates),
`clusterResources`, `project`, `shard` (negative = unset), `labels`, `annotations`
(`kubectl.kubernetes.io/last-applied-configuration` refused), `resourceVersion`, and the
write-only `config`: `username`, `password`, `bearerToken`, `tlsClientConfig{insecure, serverName,
caData, certData, keyData}`, `awsAuthConfig`, `execProviderConfig`, `proxyUrl`,
`disableCompression`.

Response (`apiclient.RegisteredCluster`): `kind: Cluster`, the object's `metadata` (managed fields
and last-applied annotation removed), `spec` without `config.execProviderConfig`, `status`
(connection state, server version, cache info, applications count, API versions, assigned shard,
conditions), `credentials{hasBearerToken, hasBasicAuth, hasCAData, hasClientCertificate,
hasExecProviderConfig}` and `inCluster` for the implicit cluster.

### Projects (resource `projects`, object = project name)

| Method | Path | Check | Behavior |
|--------|------|-------|----------|
| GET | `/projects` | `get` per item | Sorted by name; `selector`, `limit`/`continue`; token metadata normalized in responses. |
| POST | `/projects` | `create` (+`update` for upsert) | Body `{project, upsert}`. `missing payload 'project' in request`; name must be a DNS-1123 subdomain; policies normalized; `internal/project.ValidateProject` (`error validating project: …` with upstream codes: 400, `AlreadyExists` 409); `sourceNamespaces` entries must be valid globs or `/regex/`; time zones normalized; the escalation checks below apply with a bare project (same name, no labels, empty spec) as the old one, so global projects selected by the new labels count. Identical existing: returned; different: `existing project spec is different; use upsert flag to force update; difference in keys "…"`; upsert: the update path; an existing project the caller may not `get`: 409 `project '<name>' already exists`. |
| GET | `/projects/{name}` | `get` (middleware, before existence) | 404 when missing and allowed. |
| PUT | `/projects/{name}` | `update` (middleware) | Body `{project}`; name empty or equal to the path; `metadata.resourceVersion` → 409 when stale (without it conflicts are retried with fresh checks). Escalation checks (TEN-046, see "Project escalation checks"). Impact check (TEN-047): Applications of the project in the control-plane namespace that lose a permitted source or destination → `as a result of project update N applications source became invalid and M applications destination became invalid`. Writes spec, labels and annotations; token records of roles that no longer exist are dropped from status. |
| DELETE | `/projects/{name}` | `delete` | `name 'default' is reserved and cannot be deleted` (before RBAC, upstream); `project is referenced by N applications`; `project is referenced by N applicationsets` (template `spec.project`). |
| GET | `/projects/{name}/detailed` | `get` | Effective project (global projects merged), global projects (status removed), scoped read repositories the caller may `repositories, get, <project>/<url>`, scoped clusters the caller may `clusters, get, <project>/<server>` (sanitized). |
| GET | `/projects/{name}/globalprojects` | `get` | `{items}`. |
| GET | `/projects/{name}/events` | `get` | Events with `involvedObject.name/uid/namespace` of the project. |
| GET | `/projects/{name}/syncwindows` | `get` | `{windows}` active now (project's own windows); an unparsable window is 412. |
| GET | `/projects/{name}/links` | `get` | `project.links` evaluated with `project` (status emptied); denial: 403 `unauthorized access to project <name>` (upstream text). |
| POST | `/projects/{project}/roles/{role}/token` | `update` or membership of one of the role's groups | Body `{expiresIn, id, description}` (description not stored; `expiresIn` 0..100 years; id ≤ 128 characters without whitespace). The stored project must validate. Unknown role: 404 `project '<p>' does not have role '<r>'`; reused id (spec or status records): 400 `Token id '<id>' has been used. `. Token from `auth.TokenService.IssueProjectToken` (`sub proj:<p>:<r>`, `jti`, `iat`, `exp` when > 0); metadata `{iat, exp, id}` appended to `status.jwtTokensByRole[role]` (status subresource, resourceVersion precondition, retried on conflict with a re-minted token). Response `{token, id, issuedAt, expiresAt}`: the only time the token is returned. |
| DELETE | `/projects/{project}/roles/{role}/token/{iat}?id=` | same | Located by `id` when given, else by `iat`; removed from the deprecated `spec.roles[].jwtTokens` (first) and from status; unknown role or token: 200. |

#### Project escalation checks

Every project create, update and upsert enforces `update` on each object below (in this order;
the first denial is returned with the standard message):

1. On the project's own specs (old → new), `requiredGrants`:
   - each destination added, removed or changed, compared as a whole (server, name, namespace:
     widening `apps` to `*` counts) → `clusters` on every object the entry designates: its
     `server` as written (a glob is checked as the pattern string, upstream), and for a `name`
     the servers currently registered under that name, or `*` for a name glob, a negated name
     or a name no cluster has (only a grant matching every object passes);
   - each source repository added or removed → `repositories, update, <repo>`;
   - any change of `clusterResourceWhitelist/Blacklist`, `namespaceResourceWhitelist/Blacklist`
     → `clusters` on every object of every new destination;
   - each `destinationServiceAccounts` entry added, removed or changed (server, namespace,
     impersonated account) → `clusters, update, <server>`.
2. `projects, update, <global>` for every global project that a label change adds or removes
   (selectors evaluated before and after; escaping a global project also escapes its deny lists
   and sync windows).
3. `requiredGrants` again on the specs with their global projects merged
   (`internal/project.MergeVirtualProject`), so inherited destinations, sources, resource lists
   and service accounts gained or lost need the same permissions, and a change of the project's
   own resource lists covers inherited destinations too.

Token-route callers without `projects, update` get the standard denial whether the project or
role exists or not (group membership is only evaluated for callers that have groups).

Mutations annotate the audit record: Cluster targets (no Kubernetes Event), AppProject targets
with reason `ResourceCreated|ResourceUpdated|ResourceDeleted` and message `<user> created project`,
`<user> updated project`, `<user> deleted project`, `<user> created token`, `<user> deleted token`.

### Deep links (`links`)

`NewContext(Input{Resource, Application, Cluster, Project})` builds the context (`resource`,
`application` and `app`, `cluster` limited to server, name, namespaces, shard, project, labels,
annotations, `project` with status emptied, `managedByURL` only when an absolute http(s) URL);
`Evaluate` keeps links whose expr-lang condition is true, renders the URL with text/template and
sprig minus `env`, `expandenv`, `getHostByName`, and returns problem messages for the caller to
log. Rendered URLs are capped at 16 KiB.

## Invariants

- Credentials never leave the server: request credentials go to a Secret
  (`cd.patrol.example/secret-type=cluster`, `cd.patrol.example/managed-by: patrol-cd`, owned by
  the Cluster); responses carry presence flags; `execProviderConfig` is never returned; nothing
  is logged but server URLs and error texts (which never contain Secret data). Tests assert the
  absence of token, CA and exec values in raw response bodies.
- Only Secrets labeled as cluster credentials are read or written; the API refuses (412) to write
  credentials into a referenced Secret without that label, so a kubectl-created Cluster pointing
  at another Secret (e.g. `patrol-secret`) cannot be used to overwrite it.
- No existence oracle: clusters answer missing and forbidden with the same 403; project routes
  check RBAC before reading the project; token routes deny the same way for missing projects.
- Token records are written only by the token routes: create/update ignore `status` (status
  subresource) and keep the stored `spec.roles[].jwtTokens`, so a project editor can neither
  forge a record nor revive a revoked token; deleting a role drops its records, so a role
  re-created with the same name does not revive old tokens.
- Revocation is ordered (legacy spec copy first, then status) so that a partial failure never
  leaves a record that normalization could merge back.
- Writes use resourceVersion preconditions (409 for client-supplied versions, retries
  otherwise) and an in-process per-project lock (upstream `projectLock`).

## Tenant boundary for clusters (threats T1, T2, T20)

- The implicit in-cluster cluster belongs to no project and authenticates with Patrol's own
  service account. Only callers with `update` on `https://kubernetes.default.svc` (unscoped) can
  register, scope, rename or restrict it (create, upsert, update and invalidate-cache all go
  through that check); project-scoped grants (`clusters, *, team-a/*`) and `create` grants cannot.
- `awsAuthConfig` and `execProviderConfig` authenticate with the identity of the Patrol component
  that connects (AWS/GCP/Azure tokens from its environment, or a command it runs) toward a server
  URL the caller chooses. Writing them needs the action on the unscoped `<server>` object, so
  project-scoped grants cannot store them; the in-cluster server ignores both settings and is
  exempt. These are the "cluster administrators" `clusters.md` assumes for exec plugins.
- The default `Connector` (`AllowExec` false) refuses such configurations before connecting
  (`ErrExecNotAllowed`) and forces `internal/clusters` options `AWSAuthExec` and
  `DisableNativeK8sAuth`, so no in-process token source is ever built by patrol-server; such
  clusters are stored without a check and checked by the controller.

## SSRF guardrails (threat T8)

Server and proxy URLs: `http`/`https` (proxy: `http`, `https`, `socks5`), a host, no userinfo,
query or fragment; literal link-local, unspecified, multicast and cloud metadata addresses
(`169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`, `100.100.100.200`) and metadata host names
(`metadata.google.internal`, …) are refused unless `Config.AllowLinkLocal`. The default
`Connector` dials through a `net.Dialer` whose `Control` hook refuses the same addresses after
DNS resolution, and turns redirect responses into errors so credentials are never replayed to
another location (`clusters.ErrRedirect`). Loopback and private addresses are allowed (clusters live
there).

## Differences from upstream

| Upstream | Patrol | Reason |
|----------|--------|--------|
| Cluster = labeled Secret; `config` JSON in the Secret | Cluster object + owned credentials Secret (ADR-0003); API body keeps Argo's `Cluster` shape (`config.proxyUrl`) | Resource model |
| Connection check runs exec plugins and mints AWS (and k8s-auth GCP/Azure) tokens with argocd-server's identity for the caller's server URL | Neither unless `ConnectorOptions.AllowExec`; such clusters are stored without a check (the controller checks them) | No user-supplied command execution in patrol-server; no ambient cloud token sent to a caller-chosen URL |
| Any `clusters, create/update` holder (project-scoped included) may store `awsAuthConfig`/`execProviderConfig` | The action on the unscoped `<server>` object is also required | Project tenants must not run commands in, or borrow the cloud identity of, Patrol components |
| In-cluster without a Secret: `CreateCluster` creates it for any `create` holder, project-scoped included | The implicit cluster counts as existing; registering it needs `update` on the unscoped in-cluster object (an identical request returns it) | A tenant would otherwise rename/restrict/scope the shared in-cluster cluster and gain `{https://kubernetes.default.svc, *}` for its project |
| Update overwrites the whole stored object with the merged request | `updatedFields` applied to the latest object per attempt; UID/server re-checked; project re-authorized if it moved | No lost updates, no write outside the authorized project |
| Delete/rotate by name check every server against the first cluster's project | Each cluster against its own project, all before any change | Tenant boundary |
| `Cluster.Equals` ignores the project; existing clusters returned to any `create` holder | Project compared; invisible existing clusters → 409 `already exists`; credential differences reported as `Config` only | No cross-project disclosure; no credential-equality hints |
| Project create and upsert skip the update checks | Same cluster/repository permission and application-impact checks as an update | Escalation through create/upsert |
| Creating an existing project returns it or a spec diff to any `create` holder | Callers that may not `get` it receive 409 `project '<name>' already exists` | No disclosure of invisible projects |
| Escalation check counts occurrences (`difference`) | Set difference | A server listed twice and removed was unchecked |
| Escalation check compares destination servers only (`""` for name entries); ignores namespaces, `destinationServiceAccounts` and labels | Whole destination entries (names resolved, `*` for name globs/unregistered names), service accounts, global-project membership (`projects, update, <global>`) and the merged (virtual) specs | Namespace widening, `{name: '*'}`, impersonation changes and global-project opt-in/out escaped the check |
| Token create/delete validate the stored project | Only create does; revocation is never blocked | Revocation must always work |
| Token deletion: unknown role answered before authorization | Authorization first | Role enumeration |
| Scoped cluster project not checked | Must exist (`project "<p>" does not exist`) | A typo would scope the cluster to a project created later |
| Request status / `spec.roles[].jwtTokens` written by Update/Create | Ignored (status subresource; stored legacy records kept) | Token records only via token routes |
| Project finalizers from the request | Only `resources-finalizer.cd.patrol.example` kept on create | Arbitrary finalizers would block deletion |
| Links render any URL | Only absolute http(s) URLs (T15) | Script injection in the UI |
| Global projects in detailed/globalprojects include status | Status removed | Other projects' token metadata |
| `GetSyncWindowsState` error: Unknown (500) | 412 `failed_precondition` | Stored invalid window is a precondition problem |
| No pagination or selector on lists | `selector`, `limit`/`continue` (api-contract.md) | Contract |

## Gaps

- Not wired into `cmd/patrolcd` (server assembly owns `Deps`); `api/openapi/openapi.json` is
  regenerated by the orchestrator (`TestOpenAPIUpToDate` fails until then; generation with these
  routes was checked into a scratch file).
- Startup creation of `default` (TEN-002) and startup token normalization (TEN-034) belong to
  the server wiring; Event emission for audit records is `rest.LogAuditor`'s recorder;
  `--enable-k8s-event` is not implemented.
- Delete/impact checks only consider Applications/ApplicationSets of the control-plane
  namespace (upstream); ApplicationSet references inside generator templates are not counted.
- The cluster-info updater (REPO-094), cache invalidation (REPO-096) and sharding (REPO-082) are
  controller work; this API only writes what they read.
- No server-side `GetClusterInfo` cache: status comes from the Cluster object.
- Residual risks of the escalation checks: a destination server glob is checked as the pattern
  string (upstream), which is sound for `*`-only policies but not for policies using `?` or
  `[...]`; a caller allowed `*` with explicit deny exceptions can still add globs covering the
  excepted servers; a literal name destination is checked against the clusters registered now,
  so renaming another cluster to that name later (which needs `clusters, update` on it) extends
  the project; project-scoped clusters are not part of the resource-list checks (they are granted
  to the project by their registrants); `syncWindows`, `sourceNamespaces`, `signatureKeys` and
  roles of the project's own spec are not escalation-checked (upstream).

## Verification

Run on 2026-10-16 after the review follow-up (macOS arm64, Go 1.27.2, envtest kube-apiserver
1.36.0):

```bash
go build ./...                                                           # exit 0
go vet ./internal/apiserver/services/clusters/... ./internal/apiserver/services/projects/... \
  ./internal/apiserver/services/links/... ./pkg/apiclient/...           # exit 0
golangci-lint run ./internal/apiserver/services/clusters/... \
  ./internal/apiserver/services/projects/... ./internal/apiserver/services/links/...  # 0 issues
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 go test -race -count=1 \
  ./internal/apiserver/services/clusters/... ./internal/apiserver/services/projects/... \
  ./internal/apiserver/services/links/... ./pkg/apiclient/...           # ok x4; 72 tests/subtests in the three service packages pass, none skipped
go test -count=1 ./internal/apiserver/                                   # FAIL: only TestOpenAPIUpToDate (expected until regeneration)
go run ./hack/parity && go run ./hack/parity -check                     # exit 0 / exit 0
```

The review regression tests (`security_test.go`, `connector_internal_test.go`,
`escalation_test.go`) were also run against the pre-fix `handlers.go`, `connector.go`,
`validate.go`, `crud.go` and `util.go` with `go test -overlay`: `TestClusterInClusterRegistration`,
`TestClusterAmbientAuthGate`, `TestClusterPartialUpdateConcurrency`,
`TestConnectorNoAmbientIdentity` and `TestProjectEscalationWidening` all fail there and pass with
the fixes.

Tests: input validation and SSRF tables, dialer guard, redirect refusal (no credential reaches
the redirect target), exec refusal, diff keys without credential names, sanitization, pagination,
route tables, deep-link evaluation (conditions, sprig, removed functions, URL restriction, size
cap, context contents); through `pkg/apiclient` against envtest with the real token service,
enforcer and audit chain: cluster lifecycle (create with a live connection check against a TLS
destination that verifies bearer/basic credentials, Secret ownership and labels, status written,
idempotent create, diff, upsert, get by server/name/name_escaped, list with in-cluster, filters,
selector, pagination, partial and full updates, immutable server, 409, credential replacement,
invalidation, delete with Secret cleanup, in-cluster delete 404), create rejections (bad token,
unknown CA, unreachable, metadata address, unknown project, unknown fields, in-cluster disabled
and `"no"`), authorization (project-scoped objects, wrapped create denial, invisible existing
server → 409, per-item lists, identical 403 for missing/forbidden, project moves, upsert needs
update, audit of denials, anonymous 401), delete by name with an unlabeled user Secret (412 on
write, kept on delete), rotate-auth end to end (emulated token controller; old token refused
afterwards), exec clusters and in-cluster materialization; projects CRUD with validation
messages and codes, status/token records ignored, idempotent create, diff keys, upsert,
pagination, 409, name mismatch, delete rules and audit messages; authorization (lists, RBAC
before existence, escalation on update and create, upsert permission); update impact; detailed
(global merge, scoped repositories/clusters filtered by RBAC, no credentials), global projects,
events, sync windows at a fixed time, links; tokens (claims, status record, role-scoped
authorization, duplicate id, unknown role, group members, identical denials, record protection,
revocation by id and iat, legacy spec copy, 8 concurrent creations, role removal and
re-creation, invalid project blocks creation but not revocation, audit). Review follow-up:
in-cluster registration rules (project-scoped and create-only callers refused, identical request
returns the implicit cluster, admin materializes it), ambient-auth gate (exec, AWS and k8s-auth GCP
configurations refused for project-scoped grants on create/upsert/update, allowed for global
grants, not probed), connector refusal without any request reaching the server (AWS environment
credentials present), partial updates keeping concurrent renames/annotations/labels, re-authorization
after a concurrent project move, stale resourceVersion 409; project escalation table
(`requiredGrants`) and end to end: namespace widening on server and name entries, name globs and
unregistered names, service-account changes, global-project opt-in/opt-out by label on update and
create, inherited destinations and deny lists, own resource lists covering inherited destinations.
