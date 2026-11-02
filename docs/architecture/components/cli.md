# Component: `patrol` CLI core (`cmd/patrol`, `internal/cli`)

Status: implemented. Root command, global flags and environment, local configuration and
contexts, transports (direct HTTPS/HTTP, port-forward), login/logout/relogin (password and
SSO), `context`, `configure`, `version`, `completion`, plugins, and the `account`, `cert`,
`gpg`, `repo`, `repocreds`, `cluster` and `proj` (with `role`, `windows`,
`source-integrity`) command groups. `--core` (core mode, API-055) is provided by
`internal/cli/core`: see [cli-core-mode.md](cli-core-mode.md). Upstream baseline: Argo CD v3.5.5 `cmd/argocd/commands/*.go`,
`pkg/apiclient/apiclient.go`, `util/localconfig` (behavior re-implemented, no code copied).
API: [`api-contract.md`](../api-contract.md) through `pkg/apiclient`.

| Package / file | Contents |
|----------------|----------|
| `cmd/patrol` | `main`: re-exec hooks (`sandbox.MaybeHelperMain`, `jsonnet.MaybeJsonnetMain`), `cli.Main` with the registrars of `internal/cli/app` (`app`, `appset`) and `internal/cli/admin` (`admin`, adapted to the root's exit codes) |
| `internal/cli` | `root.go` (`NewRootCommand`, `Execute`, `Main`, `Registrar`), `plugin.go`, `completion.go`, `version.go`, `login.go`, `session.go` (logout, relogin, context, configure), `account.go`, `cert.go`, `gpg.go`, `repo.go` (repo, repocreds), `cluster.go`, `proj.go`, `proj_role.go`, `proj_syncwindows.go`, `proj_source_integrity.go`, `util.go` |
| `internal/cli/cmdutil` | `Factory` (streams, global options, environment lookup, clock, browser, transports), global flags and `PATROL_OPTS`, target resolution and API client construction (`client.go`), exit codes (`errors.go`), prompts, `$EDITOR` loop, kubectl flags and kubeconfig paths, build version, debug request logging |
| `internal/cli/config` | Local configuration file: types, read (permission and size checks), atomic write (0600), contexts/servers/users helpers, path resolution |
| `internal/cli/transport` | `Provider` interface, `Direct`, `Unavailable`, `PortForward` (WebSocket port-forward), `PodDialer` (the same streams as in-memory connections, used by core mode) |
| `internal/cli/core` | Core mode: `Provider` (in-process API server, in-memory transport), `Register` (installs it, refuses session commands); [cli-core-mode.md](cli-core-mode.md) |
| `internal/cli/sso` | OIDC authorization-code + PKCE login with a loopback callback, refresh-token grant, provider selection from server settings |
| `internal/cli/output` | JSON/YAML printers, tables, sanitizing of server-supplied text |
| `internal/cli/clitest` | End-to-end test harness (below), reused by `internal/cli/app` and other CLI packages |

## Global behavior

- **Exit codes.** 0 success; 1 usage errors, unknown commands/flags and upstream `log.Fatal`
  paths; 20 other failures (upstream `errors.CheckError`). Errors print as `Error: <msg>` on
  stderr (sanitized); errors carrying `ExitCode() int` / `IsSilent() bool` choose their own
  status (plugins propagate theirs).
- **Global flags** are the upstream ones with Patrol names: `--patrol-context` (hidden alias
  `--argocd-context`), `--server`, `--auth-token`, `--config`, `--plaintext`, `--insecure`,
  `--server-crt`, `--client-crt/--client-crt-key`, `-H/--header`, `--http-retry-max`,
  `--grpc-web` (accepted, ignored: JSON over HTTP), `--grpc-web-root-path` (the server's root
  path), `--port-forward`, `--port-forward-namespace`, `--core`, `--kube-context`,
  `--server-name`, `--controller-name`, `--repo-server-name`, `--prompts-enabled`,
  `--logformat` (default `json`), `--loglevel`. `--redis-name`, `--redis-haproxy-name`,
  `--redis-compress` are accepted and deprecated (no Redis, ADR-0004). Command groups
  registered through upstream `InitCommand` also take the kubectl client flags (core mode
  and `--port-forward`), as persistent flags accepted by every subcommand (upstream: the
  group command only); see `cli-core-mode.md`.
- **Environment.** `PATROL_OPTS` holds shell-quoted defaults of the root flags
  (`--flag value`, `--flag`, `--flag=value`; a non-flag token fails with `PATROL_OPTS invalid
  at '<x>'`). `PATROL_SERVER`, `PATROL_AUTH_TOKEN`, `PATROL_CORE` (default of `--core`),
  `PATROL_SERVER_NAME`,
  `PATROL_APPLICATION_CONTROLLER_NAME`, `PATROL_REPO_SERVER_NAME`, `PATROL_CONFIG` (file),
  `PATROL_CONFIG_DIR`, `XDG_CONFIG_HOME`, `KUBECONFIG`, `EDITOR`. Precedence: flag >
  `PATROL_OPTS` > environment > local configuration.
- **Argo CD migration aliases** (warned once per run): `ARGOCD_OPTS` when `PATROL_OPTS` is
  unset (its `--auth-token` key is dropped so a shared variable never leaks a token),
  `ARGOCD_SERVER` when `PATROL_SERVER` is unset, `ARGOCD_AUTH_TOKEN` when `PATROL_AUTH_TOKEN`
  is unset and only when the effective server is `ARGOCD_SERVER`. `ARGOCD_CONFIG_DIR` and
  `~/.argocd` are not read.
- **Output.** `-o json` (2-space indent) / `yaml` / `wide` / command-specific formats; tables
  use two-space column gaps; server-supplied strings are sanitized (control characters,
  line/paragraph separators and bidi overrides become spaces). Timestamps print in UTC
  RFC 3339. Prompts are written to stderr.
- **Prompts.** Optional confirmations (logout, deletes) follow `--prompts-enabled` (command
  line or `PATROL_OPTS`) else the configuration's `prompts-enabled` (default off). Mandatory
  prompts (passwords, TLS warnings at login, `cluster rm` without `-y`, `cluster add` on a
  terminal without `-y`, source-integrity update/delete without `-y`) always ask. Passwords
  are read without echo from a terminal, as a line otherwise; empty answers are asked again.
  A prompt that gets no answer (input exhausted, e.g. CI without `-y`) fails the command
  (exit 20, or 1 where upstream exits fatally); only an explicit "no" cancels with exit 0.

## Local configuration and contexts

`--config`, else `$PATROL_CONFIG`, else `$PATROL_CONFIG_DIR/config`, else
`$XDG_CONFIG_HOME/patrol/config`, else `~/.config/patrol/config`. YAML with the upstream
keys (`current-context`, `contexts`, `servers` with `plain-text`, `insecure`, `grpc-web`,
`grpc-web-root-path`, `core`, `certificate-authority-data`, client certificate data, `users`
with `auth-token`/`refresh-token`, `prompts-enabled`). Written atomically (temporary file +
rename) with mode 0600 in a 0700 directory; reading refuses modes other than 0600/0400
(`config file has incorrect permission flags ...`) and files over 4 MiB. `.prev-ctx` sits
next to the file. `User` values redact tokens in `String`/`GoString`/`LogValue`.

A context's TLS settings and credentials apply only when the effective server equals the
context's server: `--server other` never receives the stored token (hardening; upstream
sends it). A requested `--port-forward` / `--port-forward-namespace` (flag or `PATROL_OPTS`)
is the effective server `port-forward`, and `--core` is `kubernetes`: they reuse only a
context created for that mode, so a remote context's token, refresh token and CA never reach
the pod the kubeconfig selects, its expired SSO session is not refreshed through that pod,
and a `Patrol-Renew-Token` from it is not stored in the remote context (upstream applies the
current context's credentials to the forwarded port).

## Transports and authentication

- **Direct**: HTTPS by default (TLS 1.2+, system roots or the context/`--server-crt` CA,
  optional client certificate), `--plaintext`, `--insecure` (explicit only). `User-Agent:
  patrol-cli/<version>`, `-H` headers, `--http-retry-max` (GET retries on transport errors
  and 502/503/504, `pkg/apiclient`), root path.
- **Port-forward** (`--port-forward`, `--port-forward-namespace`, or a `port-forward`
  context): selects a Running+Ready pod labelled `app.kubernetes.io/name=<--server-name>` in
  the namespace (else the kubeconfig's), opens one WebSocket (`v4.channel.k8s.io`) to
  `pods/<pod>/portforward?ports=8080` per local TCP connection on `127.0.0.1:0`, and forces
  `insecure`. Implemented on `github.com/coder/websocket` (client-go's port-forward packages
  are not in the module graph). Kubeconfig paths come from the command's environment
  (`--kubeconfig`, `KUBECONFIG`, `$HOME/.kube/config`).
- **Core** (`--core`, `PATROL_CORE=true`, `login --core`): resolved through
  `transport.Provider`; `cmd/patrol` installs `internal/cli/core` (in-process API server,
  no listener, kubeconfig credentials; [cli-core-mode.md](cli-core-mode.md)). Root commands
  built without that registrar fail with `core mode not available in this build`. Core mode
  with `--server`/`PATROL_SERVER`, `--port-forward` or `--auth-token`/`PATROL_AUTH_TOKEN` is
  refused (usage error).
- **Login**: TLS probe unless `--skip-test-tls` or `--plaintext` (mandatory prompts for plain
  HTTP servers and certificate errors), password login (prompts for missing values) or
  `--sso`. Prints `'<email|name|sub>' logged in successfully` and `Context '<name>' updated`;
  the context name is `--name`, else the server (plus `/<root path>`). `--port-forward`
  stores server `port-forward`; `--core` stores `kubernetes` without contacting a server.
- **SSO**: provider from `GET /api/v1/settings` (`oidcConfig` with `cliClientID`, else the
  bundled Dex at `<url>/api/dex` with client `patrol-cli`). Authorization code with PKCE
  (S256), random state and nonce (both verified), loopback listener on
  `localhost:--sso-port` (0 picks a port; `--callback` overrides the redirect base), browser
  launch or a copy-paste URL (`--sso-launch-browser=false`), 5-minute wait. The ID token is
  exchanged at `POST /api/v1/session/oidc-exchange`; the refresh token is stored. The IdP is
  reached with the server's TLS settings only when the issuer host equals the server host.
  The implicit flow is refused.
- **Renewal**: an expired SSO session with a refresh token is renewed transparently (refresh
  grant + exchange) and the configuration rewritten; renewed local sessions sent by the
  server (`Patrol-Renew-Token`) are stored.
- **Logout** revokes the session (`DELETE /api/v1/session`, warnings on failure) and removes
  the token; **relogin** repeats password login (token kind `login`) or SSO.
- **Debug logging** (`--loglevel debug`) records method, host, path (with `user:password@`
  segments redacted), status, request ID and duration of each API request; headers, queries
  and bodies are never logged.

## Command groups

`account` (list, get, can-i, update-password with re-login, generate-token, delete-token,
get-user-info/whoami, session-token, bcrypt), `cert` (add-tls with PEM validation and
de-duplication, add-ssh `--batch` with known_hosts parsing, list with type/hostname/sort
filters, rm), `gpg` (list, get, add from armored or binary keys, rm), `repo` (add with all
credential and access flags, client-side validation, server connection test then create;
list/get with `--refresh hard`; rm), `repocreds` (add, list, rm), `cluster` (add, get, list,
set, rm, rotate-auth), `proj` (create from flags/file/URL/stdin with `--upsert`, get, list,
set, edit, delete, sources, source namespaces, destinations, allow/deny resource lists,
orphaned-ignore, signature keys (deprecated), destination service accounts; `role`,
`windows`, `source-integrity git policies`). Messages and exit codes follow upstream.

`cluster add CONTEXT` loads the kubeconfig (`--kubeconfig`, `KUBECONFIG`,
`$HOME/.kube/config`), installs `patrol-manager` (or uses `--service-account`) through
`internal/clusters`, and registers the cluster with that bearer token, the CA and server
name. The kubeconfig's client certificate and key are never sent to Patrol (hardening);
`--aws-*` and `--exec-command*` register those configurations instead. `cluster rm` removes
`patrol-manager` through the kubeconfig context named like the cluster when one exists.

## Plugins and completion

An unknown first argument `<x>` runs `patrol-<x>` from `PATH` with the remaining
arguments, the command's stdio and environment plus `PATROL_BINARY`, `PATROL_CONFIG` and
`PATROL_PLUGIN_NAME`; the plugin's exit code is returned. Built-in commands win; names with
path separators are ignored; plugins found through relative `PATH` entries are refused.
`completion bash|zsh|fish|powershell` emits the script (invalid shells: `Invalid shell
'<x>'. The supported shells are bash, zsh, fish and powershell.`, exit 1); dynamic
completion lists projects, clusters, repositories, applications (`CompleteApplications` for
`internal/cli/app`), contexts and plugin names, and is silent on errors.

## Test harness (`internal/cli/clitest`)

```go
func TestMain(m *testing.M) { os.Exit(clitest.Main(m)) } // starts envtest once per binary

e := clitest.New(t, clitest.Options{Registrars: []cli.Registrar{app.Register}})
e.LoginAs("admin")                       // or e.Login("alice") through `patrol login`
e.Run("proj", "create", "demo").Succeeded(t)
e.RunAs(e.LoginToken("alice"), "proj", "delete", "demo").Failed(t, 20, "permission denied")
```

- `Main` starts one envtest kube-apiserver with `deploy/crds` when `KUBEBUILDER_ASSETS` is
  set; without it `New` skips the test (`PATROL_REQUIRE_ENVTEST=1` makes that a failure).
- `New(t, Options)` creates a unique namespace with `patrol-cm`, `patrol-rbac-cm` and
  `patrol-secret` (accounts admin, alice (read-only), bob (projects/repositories of
  `team-a`), ci (apiKey), nobody (no grants); password `clitest.Password`), then starts the
  real `internal/apiserver` with the core routes and the projects, clusters and repositories
  services, the real `settings.Manager`, `auth.TokenService`, login authenticator,
  `rbac.Enforcer` (project role policies followed from the AppProjects), `KubeSecretStore`,
  OIDC relying party, audit capture and a JSON server log (`Env.ServerLog`). Options: `CM`,
  `RBAC`, `Secret` (merged over the defaults), `TLS` (HTTPS with a test CA, `Env.CAPEM`),
  `RootPath`, `Registrars`, `Services` (extra API services built from the Env),
  `RepoServer`, `DisableAuth`.
- `Env.Run/RunWith/RunAs` execute the real root command in-process with captured stdout,
  stderr and exit code (`Result.Succeeded/Failed/Contains/NotContains`). Runs see only the
  Env's environment: `HOME` and `PATROL_CONFIG` in a private directory, empty `PATH`, plus
  `SetEnv` and per-run `RunOptions.Env/Unset`; `RunOptions.Factory` adjusts the factory
  (clock, browser). `Runner`/`NewRunner` does the same without a server (help, plugins,
  completion, local configuration).
- Helpers: `LoginToken`, `StoreContext`, `ReadConfig`, `APIClient`, `CreateProject`,
  `GetProject`, `Repositories`, `Clusters`, `Secret`, `UpdateConfigMap`, `SetPolicy`,
  `WaitFor`, `ServerFlags`, `CAFile`.
- Fakes: `FakeRepoServer` (connection tests succeed unless the URL contains `unreachable`;
  records the tested repositories), `FakeIdP` (OIDC discovery, JWKS, redirecting
  authorization endpoint, PKCE-checked code grant and single-use refresh tokens),
  `Env.NewFakeCluster(ns)` (HTTPS front end of envtest authenticating the kubeconfig admin
  token and service-account token Secrets, with a token-controller emulation issuing
  legacy-shaped tokens), `NewFakeKubeAPI` (pod list and `pods/portforward` WebSocket bridge to
  the server), `NewCA`/`IssueCert`, `Browse` (test browser).
- Isolation: each Env has its own namespace, server, configuration directory and settings.
  Cluster-scoped objects are shared: only one test per binary may install `patrol-manager`
  into `kube-system`.

## Differences by design

| Upstream | Patrol | Reason |
|----------|--------|--------|
| gRPC with gRPC-Web fallback | JSON over HTTP; `--grpc-web` ignored | ADR-0005 |
| `ARGOCD_*` variables, `~/.config/argocd` | `PATROL_*`, `~/.config/patrol`; `ARGOCD_OPTS/SERVER/AUTH_TOKEN` as warned aliases | Independent product |
| Context token sent to any `--server` | Only to the context's own server | Credential hygiene |
| `cluster add` may store kubeconfig client cert/key | Never sent; service-account token instead | Least privilege |
| `--redis-*` flags | Accepted, deprecated, ignored | ADR-0004 |
| Logout via `/auth/logout` cookie | `DELETE /api/v1/session` | API contract |
| `completion` message lists bash, zsh and fish | Lists powershell too | Accurate message |
| Port-forward via SPDY | WebSocket `v4.channel.k8s.io` | Module graph |
| `delete-token` by issued-at only | Also `--id` | Addition |
| Implicit OIDC flow | Refused | Security |
| `--sso` reads the ID token's claims without verifying it | Login and session renewal verify the signature, issuer and expiry against the IdP's key set, and at login the nonce | Only the CLI holds the nonce, and checking it means nothing on an unverified token |
| `--port-forward`/`--core` use the current context's credentials | Only a context of that mode | Credential hygiene |

## Gaps

- `ARGOCD_CONFIG_DIR` and the legacy `~/.argocd` directory are not read.
- `patrol jsonnet-eval` and `patrol __patrol-sandbox-exec` are reserved by the re-exec hooks.

## Parity IDs

Recorded in [`docs/parity/status/cli-core.yaml`](../../parity/status/cli-core.yaml).

## Verification

See `docs/verification/log.md` (V-CLICORE-01): `go build ./...`, `go vet` and
`golangci-lint` on the CLI packages, and `PATROL_REQUIRE_ENVTEST=1
KUBEBUILDER_ASSETS=bin/envtest/k8s/1.36.0-darwin-arm64 go test -race -count=1` over
`./cmd/patrol ./internal/cli/...` (excluding other workstreams' `app` and `admin`).
