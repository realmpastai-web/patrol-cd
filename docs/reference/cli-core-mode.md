# `patrol` command reference: core mode

Core mode runs the Patrol CD API inside the `patrol` process for the duration of one
command and talks to Kubernetes with your kubeconfig credentials; no Patrol CD server is
contacted and no local port is opened. It suits installations without patrol-server and
users who should be governed by Kubernetes RBAC only. Design and security notes:
[cli-core-mode.md](../architecture/components/cli-core-mode.md).

## Enabling

| Way | Effect |
|-----|--------|
| `patrol --core <command>` | Core mode for this command |
| `PATROL_CORE=true` | Default of `--core` (`true`/`false`, any Go boolean; an invalid value fails every command) |
| `PATROL_OPTS="--core"` | Default of `--core`; wins over `PATROL_CORE` when it sets the flag |
| `patrol login --core` | Stores context `kubernetes` (`core: true`, no token) and makes it current; later commands use core mode while that context is selected and no `--server` is given |

## Kubernetes access

| Flag / variable | Meaning |
|-----------------|---------|
| `--kubeconfig`, `KUBECONFIG`, `$HOME/.kube/config` | Kubeconfig files (the command's environment) |
| `--kube-context`, `--context` | Kube context |
| `-n`, `--namespace` | Namespace of Patrol CD; else the kube context's namespace; else `patrol`. There is no separate core-namespace flag (upstream has none); `--port-forward-namespace` conflicts with `--core` |
| `--user`, `--cluster`, `--token`, `--username`, `--password`, `--proxy-url`, `--request-timeout`, `--insecure-skip-tls-verify` | kubectl client overrides |

The kubectl flags (`--kubeconfig`, `--context`, `-n`, ...) belong to the command groups
`app`, `appset`, `cluster`, `repo`, `repocreds`, `proj`, `account`, `cert`, `gpg`,
`configure` and `version`, and every subcommand accepts them, before or after the group
name. A subcommand's own flag of the same name takes precedence there: `cluster add` /
`cluster set` `--namespace` (namespaces the cluster may manage), `cluster add` / `cluster
rm` `--kubeconfig` (the cluster's kubeconfig), `repo add` / `repocreds add` `--username`,
`--password`, `--proxy-url` (repository credentials); use the kube context's namespace or
`--kube-context` with those.
| `--repo-server-name`, `PATROL_REPO_SERVER_NAME` | `app.kubernetes.io/name` of the repo-server pods (default `patrol-repo-server`) |
| `--controller-name`, `PATROL_APPLICATION_CONTROLLER_NAME` | `app.kubernetes.io/name` of the controller pods (default `patrol-application-controller`); pods owned by StatefulSet `patrol-controller` are accepted too |

Example:

```bash
kubectl config set-context --current --namespace=patrol
patrol login --core
patrol app list
patrol app sync guestbook
patrol --core -n patrol-staging app get web      # one-off, another namespace
patrol --core app list -n patrol-staging          # the flag may follow the subcommand
```

The repo-server and the controller are reached through Kubernetes port-forward with your
credentials when a command needs them (rendering, diffs, repository checks, resource
trees). See the permission table in the architecture page.

Destination clusters are reached only with your kubeconfig: a context whose cluster has the
cluster's server URL (the in-cluster address uses the current context). The credentials
stored for Patrol CD in Cluster Secrets are never used. Without such a context, `cluster
add` / `cluster set` save the change without a connection check, `cluster rotate-auth`
fails with `core mode changes a cluster only with your own kubeconfig credentials for it,
never with the credentials stored for Patrol CD: ...`, logs, events and live resources
of that cluster fail with `core mode: no kubeconfig context for destination cluster <url>`,
and a render of it (`app manifests --revision`, `app diff --revision`) uses the Kubernetes
version and API versions its status holds. With such a context, a status that lacks them is
completed from the cluster.

`--plaintext`, `--insecure`, `--server-crt`, client certificates and
`--grpc-web-root-path` have no effect in core mode (no network connection to a server).

## Refused combinations (exit 1)

| Invocation | Message |
|------------|---------|
| `--core` with `--server` or `PATROL_SERVER` | `--core cannot be combined with --server (or PATROL_SERVER): core mode talks to the Kubernetes API of your kubeconfig context, not to a Patrol CD server` |
| `--core` with `--port-forward` / `--port-forward-namespace` | `--core cannot be combined with --port-forward or --port-forward-namespace: core mode does not use patrol-server` |
| Core mode (flag or context) with `--auth-token` or `PATROL_AUTH_TOKEN` | `core mode cannot be combined with --auth-token (or PATROL_AUTH_TOKEN): it authenticates with your kubeconfig credentials and never sends a Patrol CD token` |
| `PATROL_CORE=<not a boolean>` | `PATROL_CORE: invalid boolean value "<v>"` |

## Commands not available in core mode (exit 20)

`relogin`, `account update-password`, `account generate-token`, `account delete-token`,
`account session-token`:

```
Error: <command> is not available in core mode: there are no Patrol CD sessions; the Kubernetes API authenticates and authorizes every request with your kubeconfig credentials
```

`admin dashboard --core`:

```
Error: admin dashboard is not available in core mode: Patrol CD never serves the web UI from an unauthenticated in-process server, which any local process could use with your Kubernetes credentials; run it without --core against an installation with patrol-server
```

`proj role create-token` works only when your credentials can read the session signing key
(`patrol-secret`, key `server.secretkey`).

## Output differences

- `patrol version` prints the server part from the in-process server with
  `mode: core (in-process API server in the CLI; no Patrol CD server contacted)` and without
  tool versions.
- `patrol account get-user-info` shows your Kubernetes user (`SelfSubjectReview`).
- Errors caused by Kubernetes RBAC carry the Kubernetes message, e.g.
  `Error: permission denied: appprojects.cd.patrol.example is forbidden: User "alice" cannot list resource "appprojects" in API group "cd.patrol.example" in the namespace "patrol" (HTTP 403, permission_denied)`.
- When `patrol-cm` cannot be read, a warning is logged: `core mode: ConfigMap <ns>/patrol-cm is not readable with your Kubernetes credentials; default settings apply, so diffs and generated manifests may differ from the controller's`.
- `--loglevel debug` adds the in-process server's logs.

## Test and debugging override

`PATROL_CORE_ENDPOINTS=repo-server=<url>,controller=<url>` replaces port-forwarding, and is
honored only together with `PATROL_CORE_ALLOW_ENDPOINTS=true` (otherwise every core-mode
command fails). URLs must use a loopback host or a cluster-internal name (`*.svc`,
`*.svc.cluster.local`); the internal token is read from Secret `patrol-internal-token` with
your credentials. Not a product setting.
