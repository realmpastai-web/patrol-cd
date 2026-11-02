# Component: web UI (`ui/`)

Status: foundation and feature pages implemented; the typed API client, mocks and pages follow
the published OpenAPI document `api/openapi/openapi.json` (generated from the Go route table).
Normative API: [`api-contract.md`](../api-contract.md), [ADR-0005](../adr/0005-public-api.md),
[ADR-0009](../adr/0009-authn-authz.md); machine-readable contract: `api/openapi/openapi.json`. Parity inventory:
[`08-api-cli-ui-extensions.md`](../../parity/areas/08-api-cli-ui-extensions.md) Part C and
[`06-applicationset.md`](../../parity/areas/06-applicationset.md) ASET-124..127. Visual
language: [`docs/ui/design-system.md`](../../ui/design-system.md).

## Stack

All versions are pinned exactly in `ui/package.json`; `ui/pnpm-lock.yaml` is committed.

| Concern | Choice |
|---------|--------|
| Package manager / runtime | pnpm 10 (`packageManager` field), Node ≥ 22.12 |
| Build | Vite 7, `@vitejs/plugin-react`, TypeScript 5.9 (`strict`, `noUncheckedIndexedAccess`, `verbatimModuleSyntax`) |
| UI | React 19, react-router 7 (data router, lazy route modules), @tanstack/react-query 5 |
| Styling | CSS modules + CSS custom properties (no CSS framework), inline SVG icons |
| Tests | vitest 4 + jsdom, @testing-library/react + user-event + jest-dom, MSW 2 |
| Browser tests | Playwright (`ui/e2e`, `ui/playwright.config.ts`), not part of `pnpm test` |
| Lint | ESLint 10 flat config: typescript-eslint (type-checked), jsx-a11y (strict), react-hooks |
| API types | openapi-typescript 7 over `api/openapi/openapi.json` (`pnpm gen:api`) |

zod is not used: responses are trusted to match the contract and validated by TypeScript at
compile time; add runtime validation only where untrusted shapes enter (e.g. YAML editors).

## Commands

```bash
pnpm -C ui install
pnpm -C ui gen:api          # regenerate src/api/schema.d.ts from api/openapi/openapi.json
pnpm -C ui gen:api:check    # fail when src/api/schema.d.ts is stale relative to the document
pnpm -C ui typecheck        # tsc for src and for configs/scripts/e2e
pnpm -C ui lint
pnpm -C ui test             # vitest (unit + component + MSW integration)
pnpm -C ui build            # → ui/dist (embedded by the Go server)
pnpm -C ui dev              # Vite dev server; PATROL_API_URL=https://host:port proxies /api, /auth, /extensions, /terminal
pnpm -C ui dev:mock         # dev server against MSW mocks (VITE_MOCK_API=1), no backend needed
PATROL_UI_URL=https://localhost:8080/ pnpm -C ui test:e2e
```

## Layout

```
ui/
  package.json, pnpm-lock.yaml, pnpm-workspace.yaml (pnpm settings: allowed build scripts)
  index.html              <base href="/"> rewritten by the server; no inline scripts
  vite.config.ts          base './', dist/.keep restore, dev-only MSW worker route, vitest config
  scripts/                gen-api-types.mjs (+ test): OpenAPI document → src/api/schema.d.ts
  e2e/                    Playwright specs
  dist/.keep              tracked placeholder so the Go embed pattern always matches
  src/
    main.tsx              theme init, optional mocks, router creation
    app/                  App providers, routes.tsx (route table), session.tsx (auth gating)
    api/                  client.ts, errors.ts, sse.ts, basePath.ts, types.ts, schema.d.ts (generated),
                          endpoints/*.ts (one typed function per operation), contract.test.ts,
                          queries/*.ts and hooks/*.ts (React Query hooks, keys, watch merging), queryClient.ts
    design/               tokens.css, global.css, theme.ts, icons/, hooks/, components/, status/
    shell/                AppShell, nav, breadcrumbs, shortcuts, banner, custom CSS, errors, placeholders
    pages/<area>/         one module per route (exports `Component`)
    mocks/                fixtures/, handlers/ (per area), server.ts (tests), browser.ts (dev mock)
    lib/                  storage (never throws), browser (navigation seam), url checks, document title
    test/                 setup.ts, render.tsx, openapi.ts (document lookup + JSON Schema validator for tests)
```

## Serving contract with patrol-server

- The build uses relative asset URLs (`base: './'`). The server must serve `dist/index.html`
  for every non-API, non-asset path (SPA fallback) and rewrite `<base href="/">` to the
  configured root path with a trailing slash (e.g. `<base href="/patrol/">`). Everything —
  assets, API calls (`new URL('api/v1/…', document.baseURI)`), the router basename, SSO and
  logout redirects — derives from `document.baseURI`, so one build serves any `--rootpath`.
- `index.html` contains no inline script or style, so `script-src 'self'; style-src 'self'`
  works. React sets style properties through the CSSOM, which CSP does not restrict.
- Vite empties `dist/` on build; a small plugin rewrites `dist/.keep` afterwards. `ui/.gitignore`
  re-includes `dist/` (the root `.gitignore` excludes it) and tracks only `.keep`.
- In production builds `import.meta.env.VITE_MOCK_API` is statically false, so MSW and the
  `/dev/design` gallery are not bundled.

## API types (`pnpm gen:api`)

`scripts/gen-api-types.mjs` reads `api/openapi/openapi.json` (the document patrol-server
publishes, generated from the Go route table and `pkg/apiclient` types; `--spec <file|url>`
reads another document) and writes `src/api/schema.d.ts` with openapi-typescript.
`gen:api:check` fails when the file is stale; `--emit-spec <file>` writes the normalized
document. The published document is never edited; it is normalized in memory for TypeScript
only:

- References into a schema (`#/components/schemas/Application/properties/spec/properties/source`,
  how the server reuses CRD sub-schemas) would become indexed-access types that are invalid
  through optional properties and arrays and silently resolve to `any`. Each target is hoisted
  into a named component (`ApplicationSpecSource`, `…Item` for `items`, `…Value` for
  `additionalProperties`) and the original location references it; shapes are unchanged.
- `x-kubernetes-preserve-unknown-fields` objects without properties become open maps instead
  of `Record<string, never>`.

There are no hand-declared schemas or routes any more (the provisional `api-routes.mjs` and
`api-schemas.mjs` were removed once the server published its document).

UI code imports types only through `src/api/types.ts`, which names the generated schemas
(server names; `KubernetesEvent` for the schema `Event`, which clashes with the DOM type).
Read types of CRD objects are `Stored<T>` (`metadata` required, UI-1 in
`docs/verification/integration-findings.md`); request bodies use the generated shapes
(`ApplicationInput`, `AppProjectInput`, `ApplicationSetInput`).

### Contract points the UI relies on

| Area | Contract |
|------|----------|
| Lists | `{metadata, items}`; CRD lists carry `metadata.resourceVersion` (watch start); `limit`/`continue` paginate. Operations of an application come newest first, 100 per page by default: the operation panel follows `metadata.continue` ("Load older operations"). |
| Errors | `application/problem+json`: `type` `urn:patrol-cd:problem:<code>`, `title` the HTTP reason phrase, `status`, `detail`, `code` (`ERROR_CODES` in `errors.ts`, checked against the document at compile time), `requestId` (kept on `ApiError`). Missing and forbidden ApplicationSets, clusters and repositories are both 403; detail pages say so. |
| Streams | Watch routes send `{type, object}` per `data:` (`id:` = resourceVersion); the resource-tree stream sends ADDED, MODIFIED and DELETED (empty tree) envelopes only. Every stream sends `: heartbeat` comments and ends with `event: error` + problem document on failures (429 slow consumer → reconnect; 401/403/404 → stop, 401 → login). Log routes send `LogEntry` events and a final `last: true` entry; `download=true` returns the lines as a text attachment. |
| Sync, rollback, terminate | `POST …/sync` and `…/rollback` create an Operation; `DELETE …/operation` cancels the running one. All three answer `ApplicationOperationResponse {application, operation}`; the hooks seed the operation and application caches from it. Progress comes from Operation objects (`…/operations`, `…/operations/{op}`); `status.operationState` is the Application's summary. Sync bodies are flat (`revision(s)`, `strategy`, `syncOptions: {items}` replacing the Application's options, `resources`, `retryStrategy`). |
| Repositories, credential templates | Responses are `RepositoryInfo` (`connectionState` at the top level, `hasCredentials`, `inheritedCredentials`) and `RepositoryCredentialInfo` (`hasCredentials`, non-secret `username`); never secret values. Writes send secrets only in the write-only `credentials` object; `spec.credentialsRef` is server-managed (repeated unchanged on updates, never set). |
| Clusters | Responses are `RegisteredCluster` (`credentials` flags, `inCluster`; `execProviderConfig` removed). Writes are the flat `ClusterInput`; edits send `updatedFields` so the write-only `config` is never resent. |
| Certificates | `RepositoryCertificate.certData` is plain text (PEM, or the SSH key); the list route omits it (create and delete return it). TLS `certSubType` is the key algorithm. |
| SCM providers | Writes carry `metadata` and `spec` only; every secret is a `secretRef` to a Secret created outside Patrol. The webhook URL names the provider type's receiver (`/api/webhook/github`, …; UI-6). |
| Projects | Create sends `{project, upsert}`, update `{project}`; `…/detailed` returns `ProjectDetailed` (Repository objects, RegisteredClusters). |
| Resource actions | Discovery returns parameter names only (as upstream); the action dialog asks for each value. |
| Terminal | WebSocket `/terminal` under the root path; frames `TerminalMessage` (`stdin`, `resize` out, `stdout` in). |

### Contract tests

- `src/api/contract.test.ts` maps every exported client function to operations of the
  document (`satisfies Record<ClientFunction, Entry>`, so a new function without a mapping is
  a type error; helpers that are not routes say why) and checks that every operationId is used
  by the client or listed in `unusedOperations` with a reason (today: `getVersionAlias`,
  `exchangeOIDCToken`, `ssoCallback`, `getApplicationManifestsWithFiles`). Each mapping is
  executed under `<base href="/patrol/">`: method and path template (multi-segment parameters
  per `x-patrol-multi-segment`), only declared query parameters, the CSRF header on writes,
  `Accept: text/event-stream` on streams, and the JSON body validated strictly against the
  request schema. URL builders (`ssoLoginURL`, `badgeURL`, `terminalURL`,
  `applicationLogsURL`) and the WebSocket are checked the same way.
- The same requests are answered by the default MSW handlers (`getResponse`), and each answer
  is validated against the operation's response schema (problem documents for errors, the
  `LogEntry` events of log streams). `unmockedOperations` lists what dev mock mode does not
  serve (SSO redirect, WebSocket, badge image, extension proxy).
- `src/test/openapi.ts` is the validator (type, properties, required, additionalProperties,
  items, enum, anyOf, `$ref` including pointers into schemas, date-time, pattern, lengths).
  Strict mode also rejects undeclared properties, which catches stale field names that the
  document's open objects would accept.
- Route prefixes: `/terminal` and `/extensions/{extension}/{path}` are served from the root
  path, `/api/badge` and `/api/version` without `v1`, as the document declares.

## API client (`src/api/`)

- `request<T>(path, options)` (`client.ts`): resolves against the base URL, serializes query
  parameters (arrays repeat the key, empty values are dropped), sends JSON, uses
  `credentials: 'same-origin'` (the `patrol.token` cookie), adds `X-Requested-With: patrol`
  to every non-GET/HEAD/OPTIONS request, and parses JSON or text.
- Errors (`errors.ts`): every failure is an `ApiError {status, code, detail, title, type,
  requestId, retryAfterSeconds}`. `application/problem+json` bodies supply `code`/`detail`;
  otherwise the code is derived from the status (`401 → unauthenticated`, `403 → permission_denied`,
  `429 → resource_exhausted`, `5xx → internal/unavailable`); network failures are status 0,
  `unavailable`. Server problem titles are HTTP reason phrases, so `describeError` titles every
  code itself ("The request was rejected", "It already exists", "Not possible in the current
  state", …) and shows `detail` as the message; `missingOrForbiddenMessage` explains a 403 on
  an object the server may simply not have.
- 401: unless `authRedirect: false` (session bootstrap, login, settings), the installed
  unauthorized handler runs. `SessionSync` (app root) installs one that marks the cached user
  as logged out; `RequireSession` then redirects to login with the current location as
  `return_url` (or straight to SSO when configured, de-duplicated for 5 s).
- Endpoint functions (`endpoints/*.ts`): one per route, typed with generated schemas; every
  user-controlled path value goes through `seg()` (`encodeURIComponent`) or `segs()` (keeps
  `/`, for `can-i` subresources and extension paths). Repository and credential routes take
  `{purpose: 'write'}` to use the write variants.
- SSE (`sse.ts`): `openStream()` reconnects with exponential backoff and jitter (1 s → 30 s),
  resumes with `Last-Event-ID` and an optional resume query (watches pass the latest
  `resourceVersion`), closes and reconnects when no event or heartbeat comment arrives for
  45 s (server heartbeat: 15 s, promised only for `/api/v1/stream/...`; log streams pass
  `heartbeatTimeoutMs: 0`), stops on 401/403/404 (401 triggers the login redirect), treats a
  server `event: error` problem document like a failed connection with that status, and
  ignores events from replaced connections. `onError(error, willRetry)` reports
  `willRetry: false` whenever the stream stops (fatal status, `maxRetries` exhausted); a
  normal end of the response with no reconnect to follow (non-follow logs, `maxRetries: 0`)
  is completion, not an error — the state just becomes `closed`. A follow log stream that
  reconnects repeats its request (same `tailLines`), so lines can be delivered twice. It accepts any EventSource-shaped constructor;
  the default `FetchEventSource` implements the EventSource interface over `fetch()`
  because the native EventSource cannot send `Last-Event-ID` on reconnects we initiate,
  cannot report the HTTP status of a failed connection (needed to tell an expired session
  from a network blip) and never surfaces comment heartbeats. Passing `window.EventSource`
  works, with the heartbeat timeout disabled.

## Data fetching patterns

- Use the hooks in `src/api/queries/` (`useApplications`, `useApplication`,
  `useResourceTree`, `useProjects`, `useClusters`, `useSettings`, `useUserInfo`, …). Add new
  hooks there, never `fetch` in components.
- Keys come from `queryKeys` (`queries/keys.ts`) and are hierarchical: everything for one
  application lives under `queryKeys.applications.detail(name, scope)`, so one
  `invalidateQueries` refreshes its tree, diffs and events after a mutation.
- Namespaced keys are canonical: the control-plane namespace (`settings.controllerNamespace`)
  and an absent namespace both key as `''` (`canonicalNamespace`/`canonicalScope`). Pages
  reached through `/applications/:name` pass no namespace while watch events and mutation
  results carry `metadata.namespace`; without this they would land in different entries.
  Hooks canonicalize their `scope` argument and wait for settings before fetching; code that
  writes the cache from a server object uses the object's namespace through
  `canonicalNamespace(ns, controlPlaneNamespace(queryClient))`. Never build an application or
  ApplicationSet key from a raw namespace.
- Live data: list hooks start an SSE watch after the initial list loads (from its
  `resourceVersion`) and merge events into the cached list every 500 ms
  (`mergeWatchEvents`: upsert by namespace/name in place, append new, remove deleted) while
  updating detail caches. `useResourceTree` replaces the cached tree on each stream event.
  Disable watches (`{watch: false}`) for one-shot uses such as pickers. Watched lists do not
  refetch on window focus or reconnect (`watchedListOptions`); when a list is refetched anyway
  (invalidation after create/delete, `refetch()`), the snapshot can predate events already
  merged, so the watch restarts from the refetched list's `resourceVersion` and drops the old
  watch's pending events (the new watch resends what is newer). This needs the server to
  return `metadata.resourceVersion` on lists.
- Mutations (`useSyncApplication`, `useRollbackApplication`, `useTerminateOperation`,
  `useDeleteApplication`, `useRefreshApplication`, `useRunResourceAction`, …) invalidate or
  write the affected keys. Report results with `useToast()`; show failures inline with
  `ErrorBanner`.
- Errors: render `<ErrorBanner error={query.error} onRetry={query.refetch} />` (`shell/errors.tsx`)
  for recoverable failures; it explains the error and the next step (`describeError`). Throw
  (or use `throwOnError`) only when the page cannot render at all (e.g. the object does not
  exist); the route error boundary then shows recovery guidance with the shell intact.
- Retries: queries retry twice except for permanent 4xx errors; mutations never retry.
- Route loaders are optional; prefer hooks. If a loader is used, call
  `queryClient.ensureQueryData` with the same keys so components share the cache.

## Session and authentication

- Bootstrap: `RequireSession` (in `AppShell`) waits for `GET /settings` and
  `GET /session/userinfo`. Logged-in or anonymous sessions render the shell; otherwise the user
  goes to `/login?return_url=<absolute URL>` or, when OIDC or a Dex connector is configured,
  to `api/v1/auth/login?return_url=…` (as upstream).
- Login page (`/login`): local form posting `POST /api/v1/session?cookie=true`; SSO link
  labelled "Log in via <OIDC name>", "Log in via <connector>" (one Dex connector) or "Log in
  with SSO"; hides the local form when `userLoginsDisabled`; explains when neither is
  available; shows `has_sso_error`/`sso_error`; maps 401 to "The username or password is
  incorrect.", 429/`resource_exhausted` to a lockout message with the `Retry-After` wait
  (the button stays disabled meanwhile), 0/`unavailable` to a connectivity message.
  `return_url` is honored only for same-origin URLs under the base path whose router path does
  not start with `//` or `/\` (react-router renders those as external links, e.g.
  `/.//evil.example`); anything else falls back to `/applications`.
- Logout (`/logout`, user menu): `DELETE /api/v1/session`, cached data cleared, then the
  OIDC end-session URL if returned, else the login page.
- User info (`/user-info`): username, issuer, groups; password change for local accounts
  (issuer `patrol`).

## Routing

`src/app/routes.tsx` is the single route table (paths relative to the base). Pages are
lazy-loaded modules that export `Component`. Upstream paths are kept so deep links work:

| Path | Page |
|------|------|
| `/login`, `/logout` | auth (outside the shell) |
| `/` | redirect to `/applications` |
| `/applications` (`?new=1` opens the creation panel) | applications list |
| `/applications/:name`, `/applications/:appNamespace/:name` | application details |
| `/applications/[:appNamespace/]:name/:namespace/:container/logs` | full-screen logs |
| `/applicationsets`, `/applicationsets/[:appsetNamespace/]:name` | ApplicationSets |
| `/settings` and `/settings/{repos,repocreds,certs,gpgkeys,clusters[/:server],projects[/:name],accounts[/:name],scmproviders,notifications,appearance}` | settings |
| `/user-info`, `/help` | user info, docs and help |
| `/dev/design` | component gallery (dev server only) |
| `*` | system-level UI extension page registered for the path (API-122), else 404 inside the shell |

Route `handle.crumb(params)` produces breadcrumbs; pages call `useDocumentTitle()`.
`errorElement`s: `RouteError` inside the shell (navigation stays usable), `RootError` for the
shell and auth routes.

Every route sits under a pathless root route rendering `RootLayout` (`src/app/RootLayout.tsx`).
Providers whose output can contain router links belong there, inside the router, not around
`<RouterProvider>` in `App`: `ToastProvider` lives there so a toast `action` can be a
`ButtonLink` ("View application"). `renderWithProviders` uses the same layout.

On a pathname change (search-param changes excluded), the shell moves focus to `main#main`
unless the new page already placed focus inside it, and announces the new `document.title`
in a polite live region. Pages that want initial focus elsewhere in their content just focus
it on mount.

### Adding a page

1. Create `src/pages/<area>/<Name>Page.tsx` exporting `function Component()`; keep
   page-private components and CSS modules next to it (`src/pages/<area>/…`).
2. Register it in `routes.tsx` with `lazy: () => import(...)` and a `handle` crumb. Replacing
   a placeholder keeps its route and file.
3. Start with `PageHeader` (the page's `h1`) and `useDocumentTitle`. Compose from
   `src/design`; do not add new colors or one-off controls.
4. Fetch with hooks from `src/api/queries` (add hooks/keys there); handle loading
   (`Skeleton`, `aria-busy`), empty (`EmptyState` with a primary action), error
   (`ErrorBanner`) and permission-denied states explicitly.
5. Keep shareable view state (filters, tabs, selected node) in URL search params.
6. Add MSW handlers and fixtures for the routes the page uses, and a page test.

## Mocks (MSW)

- `src/mocks/handlers/<area>.ts` exports handler arrays; `handlers/index.ts` combines them.
  Match with `*/api/v1/...` patterns so handlers work under any base path. Return errors with
  `problem(status, code, detail, headers?)` to mirror patrol-server's problem details.
- Fixtures live in `src/mocks/fixtures/` and are typed with the generated types (inline
  handler bodies use `satisfies`); they are fake data (no real accounts or secrets). The
  contract test validates what the default handlers answer against the document. Handlers
  mirror server behavior where pages depend on it: `{}` bodies for `Empty` answers, 403 for
  missing ApplicationSets/clusters/repositories, server-managed `credentialsRef`, operation
  pagination, log streams ending with `last: true`.
- `problem(status, code, detail)` writes the server's problem format (`urn:patrol-cd:problem:<code>`,
  reason-phrase title, `requestId`).
- `mockState` holds the mock session. Mock login rules: any username with a non-empty
  password signs in, except password `wrong` (401) and username `locked` (429, `Retry-After:
  30`). Tests reset it after each test.
- Tests: `src/test/setup.ts` starts `setupServer`; a request no handler matches fails, and
  fails the test that made it (`src/test/guards.ts`). Override per test with
  `server.use(http.get(...))`.
- Dev mock mode: `pnpm dev:mock` serves MSW's worker from the msw package (never copied into
  `dist/`); where service workers are unavailable it falls back to resolving `fetch` against
  the same handlers in-page.

## Test conventions

- Tests sit next to the code (`*.test.ts(x)`); scripts tests in `ui/scripts/*.test.mjs`.
- Query by role and accessible name (`getByRole('button', {name: 'Sync'})`); assert ARIA
  state (`aria-sort`, `aria-selected`, `aria-expanded`, `aria-current`) and focus
  (`toHaveFocus`) rather than classes. Drive interaction with `userEvent`, keyboard first.
- Page and flow tests render the real app with `renderApp(path)` (real routes, providers,
  MSW) and assert on `router.state.location`; component tests use `render` or
  `renderWithProviders`.
- Use fake timers only around time-based behavior (toasts, SSE backoff), with
  `userEvent.setup({advanceTimers})`.
- Full-page navigations go through `browser.assign` (`src/lib/browser.ts`); spy on it.
- No snapshot tests. Each design-system component has keyboard and ARIA assertions.
- Guards (`src/test/guards.ts`, installed by `setup.ts`) fail the test that made a problem,
  naming it: unmocked requests, unhandled rejections and uncaught exceptions, `console.error`
  output (except the error-boundary output allow-listed per file and test in
  `ALLOWED_CONSOLE_ERRORS`; tests that expect an error spy with
  `vi.spyOn(console, 'error').mockImplementation(...)`), errors jsdom reports, among them what
  it does not implement (`src/test/guards.test.ts` pins these across vitest majors), and
  elements left in `document.body` after `cleanup()`: UI mounted outside `render`/`renderApp` keeps its queries, streams and
  timers running after the test. The fetch underneath MSW refuses every request, so work that
  outlives its file cannot reach a real service on jsdom's origin `http://localhost:3000`
  ([2026-10-16 flake](../../verification/runs/2026-10-16-ui-flake.md)).
- Playwright specs in `ui/e2e` target `PATROL_UI_URL` and are not run by `pnpm test`.

## Accessibility checklist (every page)

- One `h1` (via `PageHeader`), headings in order, landmarks from the shell (skip link →
  `main#main`).
- Every control reachable and operable by keyboard; visible focus; no keyboard traps outside
  modals; focus returns to the trigger after dialogs and menus.
- Page changes are handled by the shell (focus to `main`, title announced); set a specific
  `useDocumentTitle` so the announcement names the page.
- Icon-only buttons use `IconButton` (required `aria-label`); tables have a caption;
  form fields use `FormField`.
- Status uses `HealthStatus`/`SyncStatus`/`OperationPhaseStatus`/`ConnectionStatus`, never
  color alone.
- Async results are announced (toasts, live banners); errors say what happened and what to
  do next.
- Works at 360px wide without horizontal page scrolling (wide tables scroll inside their
  wrapper), at 200% zoom, in both themes, and with reduced motion.
- Links that leave Patrol (banner URLs, deep links) are validated as http(s) and open with
  `rel="noopener noreferrer"`.

## Parity coverage of this foundation

Implemented (with tests): shell navigation, session bootstrap and version panel (API-086;
the "Something went wrong" page links no external issue tracker), login page (API-087),
appearance theme (API-107), user info with password change (API-108), instance banner
(API-110), custom styles (API-112). Placeholders naming the planned capability exist for
API-088..106, API-109, API-111, API-118 and ASET-124..126; their routes, files and API
hooks are in place. Not started: Google Analytics (API-113), badge panel (API-114),
annotations-driven UI (API-115..117), view-preference persistence beyond the theme (API-119),
UI extensions (API-120..126). The parity matrix is updated only when features are
implemented and tested.

## UI extensions (`src/extensions/`)

Patrol keeps Argo CD 3.5's extension ABI so extensions built for it load unmodified
(API-120..126, API-131, API-132). Parity status: `docs/parity/status/ui-create-appsets-ext.yaml`.

### Host contract

| Global | Value |
|--------|-------|
| `window.extensionsAPI` | `registerResourceExtension(component, group, kind, tabTitle, opts?: {icon})`, `registerSystemLevelExtension(component, title, path, icon?)`, `registerAppViewExtension(component, title, icon, shouldDisplay?)`, `registerStatusPanelExtension(component, title, id, flyout?)`, `registerTopBarActionMenuExt(component, title, id, flyout?, shouldDisplay?, iconClassName?, isMiddle?)` |
| `window.extensions` | Deprecated `{resources: {"<group>/<kind>": {component}}}`; converted once, on the first resource-tab lookup, into tabs titled `More` (API-126) |
| `window.React`, `window.ReactDOM`, `window.ReactJSXRuntime` | The host's React 19 instances: `react`, the `react-dom` main entry (not `react-dom/client`) and `react/jsx-runtime` |

- **React 19 contract.** Bundles must declare `externals: {react: 'React', 'react-dom':
  'ReactDOM', 'react/jsx-runtime': 'ReactJSXRuntime'}` so hooks and context share the host
  instance. A bundle that ships a pre-19 `react/jsx-runtime` throws a `TypeError` while it
  evaluates; it is logged as `Extension <file> failed to load:` and registers nothing, and the
  rest of the UI keeps working. `window.Moment` is **not** provided (Patrol does not ship
  moment); extensions that use it must bundle their own copy.
- **Loading.** `main.tsx` calls `initExtensions()` right after the first render is scheduled,
  as upstream loads the script at startup: it installs the globals and adds one classic
  `<script>` for `extensions.js` under the root path (the first mounted slot does the same if
  the bootstrap did not, outside vitest). The server
  concatenates every `extension*.js` of its extensions directory into that script and wraps
  each file in `try { … } catch (e) { console.error('Extension <file> failed to load:', e) }`
  (upstream behavior, API-050), so a runtime error in one file does not stop the others; a
  syntax error in any file stops all of them. With no extension installed the server answers
  an empty script; a failed fetch of the default `extensions.js` (404 from a server or proxy
  without the route, network error) also means "no extensions" and is not logged (result
  `unavailable`). `loadExtensions(urls)` also accepts several bundle URLs; each gets its own
  script tag, a fetch failure of an explicitly requested bundle or an uncaught evaluation
  error is logged with the same prefix and the other bundles still load. Pages wait at most
  `EXTENSIONS_LOAD_TIMEOUT_MS` (15 s) for bundles; later registrations still appear because
  every slot re-renders on registration.
- **Glob matching** of resource tabs follows minimatch: `*` does not match the empty core
  group, `**` matches everything including `""`. Application-level tabs registered as
  `argoproj.io`/`Application` also match Patrol's `cd.patrol.example`/`Application`.
- **Icons**: extensions name Font Awesome classes, which Patrol does not ship; every extension
  entry uses one generic glyph (`IconExtension`).
- Every extension component renders inside `ExtensionBoundary`; a render error shows
  "Something went wrong with Extension for <kind>" (resource tabs) or "Something went wrong
  with Extension <title>" instead of breaking the page.

### Mount points (for the pages that host extensions)

| Where | Export (`src/extensions`) | Usage | Mounted |
|-------|--------------------------|-------|---------|
| App bootstrap (`main.tsx`) | `initExtensions()` | Call once after `createRoot().render(...)`. | `main.tsx` |
| Shell sidebar | `useSystemExtensionNavItems()` | Returns `{to, label, icon}` items to append to `NAV_ITEMS`. | `AppShell` (sidebar and mobile drawer, after the built-in sections). `AppShell` subscribes, so the breadcrumb of an extension page also updates when a bundle registers late. |
| Catch-all route (`path: '*'`) | `<SystemExtensionRoutes fallback={<NotFoundPage />} />` | Renders the system-level extension registered for the path (page heading = title, document title `<title> · Patrol CD`), a loading state while bundles load, else `fallback`. Built-in routes always win. | `routes.tsx` (breadcrumb = extension title, else "Not found") |
| Resource details panel | `useResourceExtensionTabs({group, kind, application, resource, tree})` → `TabItem[]` (or `<ResourceExtensionTabs …/>`) | Append the items to the panel's `Tabs`. `resource` is the live object; for the Application itself pass the Application. Tabs are sorted by title. | Application details panel (`AppDetailsPanel`) |
| Application details view switcher | `useAppViewExtensions(application)` + `<AppViewExtensions application tree view={viewParam} />` | Offer each extension title as a view (`view=<title>`); `shouldDisplay` is applied. | `ApplicationDetailsPage` via `details/ApplicationViews` (see below) |
| Application status panel | `<StatusPanelExtensions application tree />` | Renders each item as a labelled group; `openFlyout()` sets `?extension=<id>` and opens a drawer with the flyout (`{application, tree}`). | `ApplicationDetailsPage` |
| Application action bar | `<TopBarExtensionActions application tree placement="middle" />` (and `"end"`) | Buttons named by the registered title; content is the extension component (`{application, tree, openFlyout}`); click opens the flyout drawer (`?extension=<id>`). | `ApplicationDetailsPage` |

Application view extensions: with none registered (or none whose `shouldDisplay` accepts the
application) the resource views render their own Tree/Pods/Network/List switcher. Otherwise
`details/ApplicationViews` renders one "Application views" switcher: the four built-in views,
then each extension title (first registration of a title wins). It owns `?view=` (built-in id
or extension title, pushed to history as upstream) and passes the built-in view to the resource
views as a controlled `view`. A title in `?view=` that is not offered falls back to the
built-in default; the `pref.cd.patrol.example/default-view` annotation may name an extension.
An extension view receives the live resource tree; the resource views (and their resource
drawer) are not mounted while it is shown.

Proxy extension calls (API-046..049) go through `callProxyExtension(name, path, application,
{method, body, …})`: it requests `/extensions/<name>/<path>` under the root path with
`Patrol-Application-Name: <namespace>:<name>` and `Patrol-Project-Name: <project>` (and the
CSRF header on writes); failures are `ApiError`s.

## Application creation, ApplicationSets, notifications and help

| Feature | Module | Mounted by |
|---------|--------|-----------|
| Creation wizard (API-091) | `src/pages/create`: `<CreateApplicationPanel open onClose initial={{repoURL, project}} onCreated />` (drawer) or `<ApplicationCreateForm />` | Applications list ("New application", `?new=1`). `initial` lets other pages (e.g. a repository) prefill it; none does yet. |
| ApplicationSets list and details (API-118, ASET-124..126) | `src/pages/applicationsets` (route modules) | Routes `/applicationsets`, `/applicationsets/[:ns/]:name`; details tabs via `?tab=` (`summary`, `applications`, `tree`, `conditions`, `events`, `preview`, `previews`, `manifest`). |
| Notifications catalog | `src/pages/notifications`: `<NotificationsCatalog />` (services, triggers with descriptions, templates; per-section loading, empty guidance and retry; `h2` sections) | `/settings/notifications` renders it under its `PageHeader`; the routed page's tests are in `src/pages/settings/SettingsPages.test.tsx`. |
| Subscription editor (NOTIF-109) | `src/pages/notifications`: `<SubscriptionEditor application headingLevel />` | Application summary panel. |
| Help (API-109, API-111) | `src/pages/help`: route module, `<ChatButton floating />` | Route `/help` ("Get help" section). `AppShell` mounts `<ChatButton floating />` once (fixed, bottom-right) on every shell page except `/help`, which already shows the link; nothing renders without `help.chatUrl`. |

Creation wizard: one Application draft is edited by the form and by "Edit as YAML" (parsed
on every change; switching back to the form is refused while the YAML does not parse, with
the error and line inline). Parsing also checks the shape of every field the form reads:
required fields a partial edit removed (`repoURL`, the hydrator's dry/sync sources) are filled
in empty so validation names them, empty values (`path:`) count as absent, and a wrong type
(`sources: abc`, `syncOptions: X=y`, `targetRevision: 1.10`) is an error naming the line and
field. Fields the form does not edit pass through unchanged. In YAML mode the editor shows the
first problem that blocks creation with its document path (e.g.
`spec.source.repoURL: Enter a repository URL.`) once the YAML has been edited. Repository fields suggest registered repositories; revisions come
from `repositories/{repo}/refs` (Git), `oci-tags` (OCI) or chart versions (`helmcharts`);
paths from `repositories/{repo}/apps`; the source type and Helm value files/parameters,
Kustomize images and plugin settings from `repositories/{repo}/appdetails`. Lookups run when a
field loses focus or matches a registered repository, never per keystroke. Multi-source
applications take ref keys (a source with a ref and no path only provides files). The source
hydrator section appears when `hydratorEnabled`; the application namespace when
`appsInAnyNamespaceEnabled`; "Replace" when `syncWithReplaceAllowed`. Upsert and Validate map
to `?upsert=`/`?validate=`; server errors are explained (already exists → turn on Upsert,
invalid spec, permission). Creation navigates to `/applications/[ns/]name`.

ApplicationSet preview: the spec (editable YAML, never saved) is rendered with
`POST /applicationsets/generate` and compared with the Applications the set owns
(`ownerReferences`, from the live application list); when they cannot be listed, with the
saved spec's own output. Only fields an ApplicationSet controls are compared (name,
namespace, labels, annotations except controller-managed ones, finalizers, spec). For large
sets, Applications are classified by comparing canonical JSON; YAML and line diffs are
computed only for rows on screen. Changed Applications are listed 50 at a time as disclosure
rows (name, change, `+added −removed` line counts), expanded up front only when at most 5
change; one diff renders 400 rows before "Show all lines"; the Existing/Desired manifests are
paged 25 Applications at a time. Health of an
ApplicationSet is `status.health`, else derived from conditions as upstream.

Preview environments (pull-request generators, also nested in matrix/merge) read the pull
request number, branch and head SHA from the generated Application's labels/annotations (key
names such as `pr-number`, `branch`, `head-sha`, any prefix), then from its name (`…-pr-42`)
and source revision. Templates should copy `{{number}}`, `{{branch}}` and `{{head_sha}}` into
labels or annotations. Links are built per provider (GitHub/GHE, GitLab, Gitea, Bitbucket
Cloud/Data Center, Azure DevOps) from the generator's `api`.

Subscription editor keys are `notifications.cd.patrol.example/subscribe.<trigger>.<service>`
(trigger without dots; service may contain dots; the name part at most 63 characters);
recipients are `;`-separated and validated (email addresses for `email*` services, no spaces
for chat services, no duplicates or empty entries). Saving sends a JSON merge patch of only
the changed keys (`null` removes one). `subscribe.<service>` and the YAML `subscriptions`
annotation are shown read-only.

### Pending mounts (requests to other workstreams)

None. Every mount requested earlier has landed:

| Request | Mounted in | Entries | Tests |
|---------|-----------|---------|-------|
| Start the extension host at startup | `ui/src/main.tsx` (`initExtensions()` after `createRoot(root).render(...)`) | API-120 | `src/main.test.tsx` |
| System-level extension pages | `ui/src/app/routes.tsx` (catch-all `<SystemExtensionRoutes fallback={<NotFoundPage />} />`) | API-122 | `src/shell/shell.test.tsx` |
| System-level extension nav items | `ui/src/shell/AppShell.tsx` + `nav.tsx` (`extensionItems` after `NAV_ITEMS`) | API-122 | `src/shell/shell.test.tsx` |
| Application view extensions | `ui/src/pages/applications/details/ApplicationViews.tsx`, used by `ApplicationDetailsPage.tsx` | API-123 | `src/pages/applications/details/ResourceViewsSlot.test.tsx` |
| Floating chat button | `ui/src/shell/AppShell.tsx` (`<ChatButton floating />`, not on `/help`) | API-111 | `src/shell/shell.test.tsx` |
| Notifications catalog | `ui/src/pages/settings/notifications/NotificationsPage.tsx` | spec item: triggers with descriptions | `src/pages/settings/SettingsPages.test.tsx` |
