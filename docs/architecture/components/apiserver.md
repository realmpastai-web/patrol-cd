# Component: `internal/apiserver` (framework and core routes) and `pkg/apiclient`

Status: implemented (framework, session/SSO/account/settings/version routes, server
lifecycle, Go client base). Resource services (applications, projects, repositories, ...)
are registered by other packages through the route table.
Decision records: [ADR-0005](../adr/0005-public-api.md), [ADR-0009](../adr/0009-authn-authz.md).
Normative contract: [`api-contract.md`](../api-contract.md). Generated document:
[`api/openapi/openapi.json`](../../../api/openapi/openapi.json).

| Package | Contents |
|---------|----------|
| `internal/apiserver/rest` | Route table, typed JSON handlers, request decoding, RFC 9457 problems, per-route chain (authentication, CSRF, login rate limit, RBAC, audit), process middleware (recovery, request IDs, access log, security headers, body cap), Server-Sent Events, metrics |
| `internal/apiserver/openapi` | OpenAPI 3.1 generator from the route table (invopop/jsonschema + CRD schemas) |
| `internal/apiserver/core` | Session, SSO (OIDC login/callback/CLI exchange, Dex proxy), account, settings and version handlers; `patrol-secret` store |
| `internal/apiserver` | Server assembly: route table composition, settings following (RBAC policy, signing keys), UI serving, `/healthz`, `/metrics`, TLS, graceful shutdown; OpenAPI document builder; project-token registry and account lookup adapters |
| `internal/apiserver/cmd/genopenapi` | `go run ./internal/apiserver/cmd/genopenapi [-check]` writes (or verifies) `api/openapi/openapi.json` |
| `pkg/apiclient` | Go client: options (Argo-compatible flag names), `Do`, `DoRaw`, `Stream`/`Watch` (SSE), problem errors, session/account/settings/version methods; request/response types shared with the server |

## Route table

A `rest.Route` declares method, Go 1.22 `ServeMux` pattern, operation ID, summary, tags,
access rule, request/response Go types, stream flag, body cap, rate limiting, audit
policy, extra documented responses, raw request body documentation and OpenAPI extensions.
The same table drives routing (`Table.Mount`) and the OpenAPI document, so the two cannot
drift; the only exception are documentation-only routes (`DocumentationOnly`) for
endpoints mounted outside the table (the SCM webhook receiver), which `Mount` skips.

```go
rest.Register(t, rest.Route{
	Method: http.MethodGet, Path: "/api/v1/account/{name}", OperationID: "getAccount",
	Access: rest.RBACInHandler(rbac.ResourceAccounts, rbac.ActionGet, "{name}"),
}, h.getAccount) // func(ctx, *apiclient.GetAccountRequest) (*apiclient.Account, error)
```

Access rules: `Public()` (a presented token is still verified best effort),
`Authenticated()`, `RBAC(resource, action, objectDoc, objectFunc)` / `RBACPathValue`
(enforced by the middleware) and `RBACInHandler` (the handler calls `rest.Authorize`
because it needs state such as the Application's project or a self exemption). List
handlers filter with `rest.Can`. `SkipAuth` (public only) ignores credentials entirely
(Dex proxy).

Request binding (`pkg/apiclient` doc): `path:"x"` fields bind wildcards (`{x...}` takes
the rest of the path; single-segment values may contain `%2F`), `query:"x"` fields bind
query parameters (pointers for presence, slices repeat the key, a bare `?flag` is true),
`body:"true"` marks a whole-object body; other JSON-visible fields form the body. Bodies
must be `application/json` (or `+json`), are capped (default 4 MiB per route, 64 MiB
globally), reject unknown fields and trailing data, except Kubernetes objects
(`runtime.Object`), which accept unknown fields of newer API versions. Registration
validates operation IDs, patterns, path/field agreement and GET/DELETE bodies; a table
with errors refuses to mount.

Resource packages add routes with a `apiserver.Service` (`Register(*rest.Table)`) passed
in `Deps.Services`, and list themselves in `apiserver.DocumentedServices()` so the
OpenAPI document covers them.

## Request chain

Process-wide (outermost first): panic recovery (500 problem, server keeps serving) →
request ID (`X-Request-Id`, incoming value kept when it matches `[A-Za-z0-9._-]{1,64}`)
→ access log (request ID, method, route pattern, status, bytes, duration, client,
actor; never query strings, headers, bodies or tokens) → security headers
(`X-Content-Type-Options: nosniff`, `Referrer-Policy: same-origin`, `X-Frame-Options`
default `sameorigin`, `Content-Security-Policy` default `default-src 'self';
frame-ancestors 'self'`, HSTS on TLS) → body cap.

Per route: authentication → CSRF → login rate limit → RBAC → handler → audit.

- Authentication: `auth.TokenFromRequest` (bearer header wins over the `patrol.token`
  cookie chunks), `auth.TokenService.Verify` (signature/kid, expiry, revocation,
  account enabled/capability, API token still listed, password not changed since issue,
  project token still recorded, IdP tokens through `SSO.ExternalVerifier`). Missing token
  → 401 `no session information`; invalid → 401 `invalid session: <cause>` and a stale
  cookie is cleared; revocation-store or lookup failures → 503 (never accepted). With
  `users.anonymous.enabled` a missing or invalid token yields the anonymous identity
  (RBAC `policy.default`). Login sessions within 5 minutes of expiry are renewed:
  browsers get a new cookie, bearer clients the `Patrol-Renew-Token` header (adopted by
  `pkg/apiclient`). Routes that end or invalidate the session (`DELETE /session`,
  `PUT /account/password`) set `Route.NoRenew` and never carry a replacement token.
  `--disable-auth` skips authentication and RBAC.
- RBAC subjects: login/API tokens → the account name; project tokens → the verified
  project role only; SSO/IdP → `rbac.SubjectsFromClaims` with the `scopes` of
  `patrol-rbac-cm` (default `groups`).
- CSRF: cookie-authenticated non-GET/HEAD requests need `X-Requested-With: patrol`, and
  an `Origin` (or, without it, `Referer`) naming the request host, `url` or an
  `additionalUrls` entry; `null` origins are rejected. Bearer requests are exempt.
- Login rate limit: per client IP token bucket (5/s, burst 30, LRU of 10 000 IPs;
  `X-Forwarded-For` only from configured trusted proxies) → 429 with `Retry-After`;
  per-account lockout, the global concurrency limit and timing equalization stay in
  `internal/auth`.
- Denials: 403 `permission denied: <resource>, <action>, <object>, sub: <subject>, iat:
  <RFC 3339>` (upstream shape).
- Audit (`rest.Auditor`; default `rest.LogAuditor`): one structured `audit` line per
  mutating request and per denial (also on GET): request ID, actor and kind, operation,
  route pattern, RBAC triple, result (`succeeded`/`failed`/`denied`), status, reason,
  target. Handlers annotate the record (`rest.AuditFrom(ctx).SetTarget/SetMessage`).
  Targets of kind `Application`/`AppProject` also get a Kubernetes Event
  (`ResourceCreated|Updated|Deleted`, `AccessDenied` as Warning) through the injected
  `EventRecorder`. Bodies, tokens and passwords are never recorded.

Errors are `application/problem+json` `{type, title, status, detail, instance, code,
requestId}` with `type = urn:patrol-cd:problem:<code>`. `rest.ProblemFor` maps
`*rest.Error`, Kubernetes status errors (NotFound 404, AlreadyExists/Conflict 409,
Forbidden 403 without the server's own identity, Invalid 422, BadRequest 400,
TooManyRequests 429 + Retry-After, Gone/Expired 410, timeouts 503), RBAC errors (403),
token errors (401), body limits (413), deadlines (504) and everything else to a 500 whose
detail is `internal error` (cause logged with the request ID). Unmatched paths are 404,
other methods 405 with `Allow`.

## Server-Sent Events

`rest.RegisterStream` / `rest.ServeSSE`: `text/event-stream`, `data: <json>` per event
with optional `id:`/`event:`, `: heartbeat` comments (default 15 s), flush per write,
write deadline (30 s), per-event `Authorize` callback (rejected events are skipped),
credential re-verification (default every minute; a revoked or expired session ends the
stream with `event: error` 401), a bounded queue (default 256) whose overflow ends the
stream with `event: error` 429 `resource_exhausted` (the producer gets
`ErrSlowConsumer`), producer panics → `event: error` 500, client disconnect cancels the
producer. `pkg/apiclient.Client.Stream` reads events (bounded size, `Last-Event-ID`) and
`apiclient.Watch` decodes `{type, object}` watch events.

## Core routes

| Route | Behavior |
|-------|----------|
| `POST /session` | Local login via `auth.Authenticator` (lockout, concurrency limit, timing equalization); every failure that could reveal account existence is 401 `Invalid username or password`; disabled account / missing `login` capability after a correct password are reported; `users.session.duration` lifetime; `?cookie=true` sets the cookie; metrics `patrol_server_login_requests_total{method,result}`; audit actor is the account only on success |
| `DELETE /session` | Revokes login and SSO sessions until expiry (`patrol-session-revocations`), clears the cookie, returns `logoutURL` for SSO sessions when `oidc.config.logoutURL` is set; API and project tokens are revoked by deleting their metadata, not by logout |
| `GET /session/userinfo` | `{loggedIn, username, iss, groups}`; anonymous → `loggedIn: false` |
| `GET /auth/login` | Validates `return_url` against `url`/`additionalUrls`, state/nonce/PKCE in the encrypted `patrol.oauthstate` cookie, 303 to the IdP; redirect URI `<url>/api/v1/auth/callback` chosen per request host |
| `GET /auth/callback` | Clears the state cookie, completes the code flow, mints a Patrol SSO session (claims named by `scopes` travel in it), sets the cookie, 303 to the return URL; audited |
| `POST /session/oidc-exchange` | CLI SSO: verifies the IdP ID token (signature, issuer, audience incl. `cliClientID`, expiry, `iat` within 5 min), userinfo groups when `enableUserInfoGroups` (access token required), returns a Patrol session |
| `/api/dex/...` | Reverse proxy to the bundled Dex when `dex.config` is set (external URL path preserved; headers forwarded as upstream, including Dex's own `Authorization` credentials such as client_secret_basic and Dex access tokens; `patrol.token` cookies and Patrol-issued bearer tokens (`iss: patrol`) stripped; Dex 500 → 303 `<basehref>/login?has_sso_error=true`); server-side discovery of the Dex issuer goes to the internal Dex address; `core.DexTransport` for strict TLS |
| `GET /account`, `GET /account/{name}` | Accounts the caller may get; local callers always see themselves; RBAC is checked before existence (no oracle) |
| `GET /account/can-i/{resource}/{action}/{subresource...}` | Validates resource/action, normalizes `<project>/<ns>/<name>` for the control-plane namespace, `{value: yes|no}` |
| `PUT /account/password` | Upstream rules: self (local) without RBAC, otherwise `accounts, update, <name>`; local callers prove their own current password through the throttled login path; SSO sessions must be younger than 5 min; project tokens refused; `passwordPattern`; earlier sessions and API tokens of the account stop verifying |
| `POST /account/{name}/token`, `DELETE .../token/{id}` | Self or `accounts, update`; `apiKey` capability; duplicate ID → 409 `already_exists`; unknown → 404; `patrol-secret` updated with resourceVersion optimistic concurrency (`retry.RetryOnConflict`, the token is re-minted on each attempt) |
| `GET /settings` | Public subset; logged in or auth disabled adds banner, `controllerNamespace`, `resourceOverrides`; logged in adds `passwordPattern`; never secrets |
| `GET /settings/plugins` | Authenticated; repo-server `ListPlugins` (503 `failed to list sidecar plugins from repo-server`) |
| `GET /version`, `GET /api/version` | Only `version` for callers that are not logged in unless anonymous access or `--disable-auth`; tool versions computed once (`core.ExecToolVersions`) |

## Server lifecycle

`apiserver.New(Config, Deps)` composes the table (core + `Deps.Services`), subscribes to
settings (RBAC `ApplyConfigMap`, `TokenService.SetKeys`), and builds the handler:
`/healthz` (also at the root under `--rootpath`; 503 while not `Ready` or shutting down),
matched API routes, 404/405 problems for unmatched `/api/...`, otherwise the UI
(`UIHandler`: files from an `fs.FS`, `assets/` immutable-cached, everything else
revalidated, `index.html` with injected `<base href>` for HTML requests (history routing),
dot-files never served; `OverlayFS` for `--staticassets`). `--rootpath` mounts everything
under the prefix; `--basehref` defaults to it. `Run`/`Serve`: HTTPS by default with the
certificate from `patrol-secret` (`tls.crt`/`tls.key`, read per handshake so rotation needs
no restart) or a generated ECDSA self-signed certificate, configurable min/max version and
TLS 1.2 cipher suites (insecure suites rejected); plain HTTP on the TLS port gets 307 to
https (first-byte sniffing); `--insecure` serves plain HTTP; `/metrics` on its own listener;
graceful shutdown (healthz 503, `http.Server.Shutdown`, streams canceled after a 2 s
grace). The TLS and redirect servers share one socket through the first-byte splitter,
whose `Close` is idempotent so a clean shutdown returns nil.

Adapters for wiring: `apiserver.AccountLookup(settings)` (`auth.AccountLookup`),
`apiserver.ProjectTokenRegistry(get)` (`auth.TokenRegistry` on
`AppProject.status.jwtTokensByRole`: by `jti`, legacy by `iat`; missing project or role →
unknown), `core.KubeSecretStore`, `core.NewSSO(...).ExternalVerifier(settings)`.

## Metrics

`patrol_server_http_requests_total{operation,method,code}`,
`patrol_server_http_request_duration_seconds{operation,method}`,
`patrol_server_http_inflight_requests`, `patrol_server_login_requests_total{method,result}`,
`patrol_server_sse_streams{operation}`, `patrol_server_sse_slow_consumers_total{operation}`,
`patrol_server_audit_events_total{result}`,
`patrol_server_authorization_denials_total{operation}`,
`patrol_server_rate_limited_requests_total{operation}`, `patrol_server_handler_panics_total`;
registered on the registry passed in `Deps.Registry`.

## OpenAPI

`openapi.Generate` reflects request/response types with invopop/jsonschema; a mapper
gives Kubernetes leaf types their wire schemas (`metav1.Time`/`MicroTime` date-time,
`metav1.Duration` string, `intstr.IntOrString` and `resource.Quantity` int-or-string,
`runtime.RawExtension`/`apiextensionsv1.JSON`/`unstructured.Unstructured` preserve-unknown,
`ErrorCode` enum). CRD kinds (Application, AppProject, ApplicationSet, Repository,
RepositoryCredential, Cluster, SCMProvider, Operation) use the `openAPIV3Schema` of
`deploy/crds/*.yaml`; every `api/v1alpha1` struct reachable from them is a JSON-pointer
reference into that schema (duplicated sub-schemas are deduplicated). Operations carry
`x-patrol-access`, `x-patrol-rbac` (resource, action, object, enforcedBy),
`x-patrol-rate-limited`, `x-patrol-stream`, plus a route's own `Route.Extensions` (`x-`
keys only; a collision with a generated field is an error). `SkipAuth` routes document
`security: []`. Output is deterministic;
`internal/apiserver/server_test.go:TestOpenAPIUpToDate` fails when the checked-in file is
stale.

- Request vs. response objects: the CRD schemas leave `metadata` optional because they also
  describe request bodies. Responses, stream events, every type reflected for them and the
  `<Kind>List.items` reference `Stored<Kind>` (`allOf: [<Kind>]`, `required: [metadata]`;
  e.g. `StoredApplication`), so generated clients see `metadata` as always present; request
  bodies keep `<Kind>`. A Go type holding a CRD object cannot be both a request and a
  response type (generation fails with "generated twice with different content").
- Streams (`Route.Stream`): the `text/event-stream` schema is the data of the default events;
  the media type's `x-patrol-events` lists `message` (that schema) and `error` (`Problem`,
  ends a failed stream). A `ResponseDoc` with the success status adds a representation to the
  same response instead of replacing it (logs: `application/octet-stream` with
  `download=true` next to the `LogEntry` stream).
- Raw request bodies: a route whose handler reads the body itself documents it with
  `RequestContentType` (+ `RequestDescription`, `RequestBodyOptional`) as a media type without
  a schema (OpenAPI 3.1 binary content): `application/gzip` for
  `getApplicationManifestsWithFiles`, an optional `*/*` body for the non-GET extension proxy
  operations.
- Enums: `JSONSchemaEnum()` on string types (`ErrorCode`, path parameters) and
  `jsonschema:"enum=..."` tags (watch event `type`: ADDED, MODIFIED, DELETED; the streams
  send no bookmark or error watch events, failures are `event: error`;
  `TerminalMessage.operation`: stdin, stdout, resize, with `data` optional because resize
  frames omit it). The generator tests compare them with the `apiclient` constants.
- Documentation-only routes (`Route.DocumentationOnly`): listed in the document, never
  mounted (`Table.Mount` skips them; no handler needed). `services/webhooks` (in
  `DocumentedServices`) documents `POST /api/webhook` and `POST /api/webhook/{provider}`,
  which `internal/server` mounts as hidden raw routes serving `internal/webhook`: the
  `provider` enum (receivers `azuredevops`, `gitea`, `gogs`, `github`, `gitlab`,
  `bitbucket`, `bitbucketserver`, `ghcr` and the aliases `azure-devops`, `bitbucketcloud`,
  `bitbucket-cloud`, `bitbucket-server`; matched case-insensitively), plain-text error
  responses, and `x-patrol-webhook-receivers` (per receiver: kind, required and excluded
  headers, authentication header, patrol-secret keys, SCMProvider types). Its tests check the
  names, aliases and headers against the real `webhook.Handler`.

## Security notes

- Tokens, passwords, client secrets and request bodies are never logged or audited;
  unknown usernames of failed logins are not logged (often mistyped passwords).
- Problem details of 5xx are generic; Kubernetes Forbidden messages (which describe the
  server's service account) are not returned.
- Cookies: `patrol.token` HttpOnly, SameSite=Strict, Secure by the rule in
  [auth.md](auth.md#security-notes); state cookie SameSite=Lax, short-lived, encrypted.
- RBAC is checked before existence for account reads; SSE re-checks credentials and
  authorizes every event.
- The Dex proxy never forwards Patrol credentials (`patrol.token` cookies, Patrol-issued
  bearer tokens); Dex's own credentials in `Authorization` pass through.
- Logout and password-change responses never carry a renewed session token.

## Parity IDs covered

See `docs/parity/status/apiserver.yaml`: API-005, API-010, API-013, API-016, API-040,
API-041, API-042, TEN-085, TEN-089, TEN-096, TEN-097, TEN-101, TEN-103, TEN-104, TEN-106,
TEN-107, TEN-113, TEN-115, TEN-127, TEN-129 (implemented); API-001, API-002, API-003,
API-004, API-008, API-009, API-011, API-012, API-015, TEN-052, TEN-081, TEN-108, TEN-121,
TEN-123, TEN-128 (in progress).

## Differences and gaps

| Upstream | Patrol | Reason |
|----------|--------|--------|
| gRPC, gRPC-Web and REST gateway multiplexed with cmux | JSON over HTTP only; plain HTTP on the TLS port still redirects | ADR-0005 |
| `SessionService.Delete` only clears the cookie; `/auth/logout` revokes | `DELETE /session` revokes login/SSO sessions and returns the IdP logout URL | ADR-0009; one logout path for UI and CLI |
| SSO cookie holds the IdP ID token | Patrol SSO session; CLI exchanges its ID token (`/session/oidc-exchange`) | ADR-0009 |
| `renew-token` gRPC header | `Patrol-Renew-Token` response header / refreshed cookie | no gRPC |
| SSO config change restarts the server | provider rebuilt on the next request after a settings change | no restart needed |
| Content-type allow-list flag (`--api-content-types`) | JSON (`application/json`, `+json`) required for request bodies; 415 otherwise | CSRF defense is the `X-Requested-With` header |

Not implemented here: `/auth/logout` redirect endpoint (TEN-108), `{{token}}` in the
logout URL (the IdP ID token is not kept), `fields=` projection and gzip (API-003), log
download (API-004, logs owner), minimum client version (API-006), swagger endpoints
(API-012), tracing spans, `/download/patrol-*` (API-014), implicit grant, server-side
OIDC refresh (TEN-125), Azure-specific OIDC (TEN-126). `logoutURL` placeholders are
query-escaped (auth component). Default CSP is stricter than upstream (`frame-ancestors
'self'` only); an absolute `ui.cssurl` needs a custom `--content-security-policy`.

## Verification

Run on 2026-10-16 (macOS arm64, Go 1.27.2, envtest kube-apiserver 1.36.0):

```bash
go build ./...                                                   # ok
go vet ./internal/apiserver/... ./pkg/apiclient/...              # ok
golangci-lint run ./internal/apiserver/... ./pkg/apiclient/...   # 0 issues
go run ./internal/apiserver/cmd/genopenapi -check                # ok (document up to date)
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 ./internal/apiserver/... ./pkg/apiclient/...
# ok internal/apiserver, internal/apiserver/core, internal/apiserver/openapi,
#    internal/apiserver/rest, pkg/apiclient; 148 tests/subtests pass, none skipped
go run ./hack/parity                                             # ok; apiserver.yaml merged, no problems reported
```

UI → API contract requests UI-1 … UI-6 (`docs/verification/integration-findings.md`), run
2026-10-16 (macOS arm64, Go 1.27.2, Node 22.22.3, pnpm 10.33.0, envtest 1.36.0):

```bash
go build ./...                                                   # exit 0
go vet ./internal/apiserver/... ./pkg/apiclient/...              # exit 0
golangci-lint run ./internal/apiserver/... ./pkg/apiclient/...   # 0 issues
go run ./internal/apiserver/cmd/genopenapi                       # twice: identical output (sha256 0caa434c…)
go run ./internal/apiserver/cmd/genopenapi -check                # exit 0
KUBEBUILDER_ASSETS=$PWD/bin/envtest/k8s/1.36.0-darwin-arm64 \
  go test -race -count=1 ./internal/apiserver/... ./pkg/apiclient/...
# exit 0: every package with tests ok (including the new services/webhooks);
#   652 tests/subtests pass, none fail or skip (go test -json)
pnpm -C ui gen:api                                               # twice: identical output
pnpm -C ui gen:api:check                                         # up to date
pnpm -C ui typecheck && pnpm -C ui lint && pnpm -C ui build      # exit 0 each
pnpm -C ui test   # 3 runs. Run 1: exit 1, contract.test.ts "answers every called operation ..."
                  # timed out (10 s default) at load average ~330; run 2: exit 0 (76 files, 911
                  # tests); run 3 (after the UI flake work gave that test a 60 s timeout): exit 0
                  # (76 files, 912 tests)
```

`go run ./hack/parity -check` fails even right after regeneration: the generator orders the
implementation and test lists of alias-merged rows (APP-038, APP-077, DIFF-076, SYNC-046,
SYNC-082) nondeterministically. The rows of this component are stable.

Tests: route validation table, binding/decoding and error table, client↔server round-trip
property test (rapid), authentication matrix (no/garbage/forged/expired/revoked token,
API token listed and deleted, project token recorded and not, password changed, disabled
account, cookie, bearer precedence, public routes, anonymous, disabled auth), renewal
(header and cookie; none on `NoRenew` routes), CSRF table, RBAC denial with audit entry and Events
(`record.FakeRecorder`), rate limiting, panic recovery, problem mapping table, request
IDs, security headers, access-log redaction, SSE (heartbeats, per-event authorization,
client disconnect, revoked session mid-stream, slow consumer, producer error/panic);
core routes through `pkg/apiclient` against a real `auth.TokenService`, `rbac.Enforcer`
and settings snapshot: login/lockout/cookie/logout revocation, no renewed token on logout or password change
inside the renewal window, userinfo, accounts,
can-i, token lifecycle, password changes, settings public vs authenticated, plugins,
version, OIDC code flow with PKCE against an in-process IdP, callback errors, CLI
exchange, userinfo groups, IdP bearer tokens, Dex proxy (Patrol credentials stripped,
Basic client credentials and Dex bearer tokens forwarded); `KubeSecretStore` conflict
retries (fake clientset) and concurrent updates against envtest; server: OpenAPI
staleness, golden core route table, root path/base href/UI/cache headers, settings
changes (RBAC, key rotation), TLS with self-signed certificate, HTTP→HTTPS redirect, TLS
1.1 refusal, metrics port, graceful shutdown with an open stream and without in-flight
requests (repeated, nil error), idempotent split-listener close, readiness, project
token registry; OpenAPI generator (leaf mappings, CRD references, determinism, every
`$ref` of the checked-in document resolves, `Stored<Kind>` response schemas and the
request/response conflict error, raw request bodies, stream events and merged
representations, watch/terminal enums against the `apiclient` constants, `security: []`
and extensions of documentation-only routes); documentation-only routes are not mounted;
`services/webhooks` names, aliases (case-insensitive), required/excluded headers and
SCMProvider type mapping against the real `webhook.Handler`.
