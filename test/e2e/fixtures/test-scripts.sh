#!/usr/bin/env bash
# Offline regression test for the kind tooling. Starts two envtest kube-apiservers (no Docker)
# whose kubeconfig contexts are named kind-patrol-control and kind-patrol-target, points
# PATROL_KIND_CACHE at them, and exercises:
#   - the hack/kind/lib.sh guards (cluster names, context names, non-local servers);
#   - PATROL_WAIT_SCALE validation and patrol::secs, the digest pins of the registry, kind node and
#     oras images, patrol::docker_pull_retry and the hack/kind/load-image.sh fallback (with fake
#     docker, kind and sleep commands first on PATH);
#   - the polling helpers of hack/kind/verify-install.sh (until_true, time_left) and its comparison
#     and sync waits with their shared deadlines, both taken from the script and run with a fake
#     clock (sleep advances $SECONDS instead of waiting) and stubs for the kubectl and API calls;
#   - hack/kind/register-target.sh (CRD install, ServiceAccount + TokenRequest in the target,
#     Secret + Cluster in the control plane; the stored token authenticates against the target
#     with TLS verified by the stored caData);
#   - test/e2e/fixtures/patrol/register.sh (credential copies, Repository/Application objects);
#   - the checks hack/kind/restore-drill.sh runs before it deletes the namespace (finalizers left,
#     Operations in progress, controller pods, failures of the API server, the objects recorded on
#     patrol-target, the objects of a backup), taken from the script;
#   - hack/kind/env.sh.
# The envtest servers stand in for the kind clusters, so this is labelled "simulated" in
# docs/verification; the real environment is checked by test/e2e/fixtures/smoke.sh.
#
# Usage: test/e2e/fixtures/test-scripts.sh   (KUBEBUILDER_ASSETS defaults like validate.sh)
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
SIM=$(mktemp -d)
mkdir -p "$SIM/cache"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
export KUBEBUILDER_ASSETS=${KUBEBUILDER_ASSETS:-$ROOT/bin/envtest/k8s/1.36.0-$os-$arch}
# shellcheck source=test/e2e/fixtures/lib/envtest.sh
source "$ROOT/test/e2e/fixtures/lib/envtest.sh"
cleanup() {
	envtest_stop
	rm -rf "$SIM"
}
trap cleanup EXIT

envtest_start "$SIM/control" kind-patrol-control
cp "$ENVTEST_KUBECONFIG" "$SIM/cache/patrol-control.kubeconfig"
CONTROL_SERVER=$ENVTEST_SERVER
envtest_start "$SIM/target" kind-patrol-target
cp "$ENVTEST_KUBECONFIG" "$SIM/cache/patrol-target.kubeconfig"
TARGET_SERVER=$ENVTEST_SERVER
cp "$SIM/cache/patrol-target.kubeconfig" "$SIM/cache/patrol-target-internal.kubeconfig"
kubectl config --kubeconfig "$SIM/cache/patrol-target-internal.kubeconfig" set-cluster kind-patrol-target \
	--server https://patrol-target-control-plane:6443 >/dev/null
echo "control=$CONTROL_SERVER target=$TARGET_SERVER"
export PATROL_KIND_CACHE=$SIM/cache

pass=0
fail=0
expect() { # expect ok|fail DESCRIPTION -- CMD...
	local want=$1 desc=$2
	shift 3
	local got=ok
	"$@" >"$SIM/out" 2>&1 || got=fail
	if [[ $got == "$want" ]]; then
		pass=$((pass + 1))
		printf 'PASS  %-62s (%s) %s\n' "$desc" "$got" "$(tail -n 1 "$SIM/out")"
	else
		fail=$((fail + 1))
		printf 'FAIL  %-62s want %s got %s\n' "$desc" "$want" "$got"
		sed 's/^/      /' "$SIM/out"
	fi
}
lib() { bash -c "source '$ROOT/hack/kind/lib.sh'; $*"; }

echo "== guard functions"
expect ok "guard_context kind-patrol-control" -- lib patrol::guard_context kind-patrol-control
expect ok "guard_context kind-patrol-target" -- lib patrol::guard_context kind-patrol-target
expect fail "guard_context kind-eks-worker-1 (existing non-patrol context)" -- lib patrol::guard_context kind-eks-worker-1
expect fail "guard_context docker-desktop" -- lib patrol::guard_context docker-desktop
expect fail "guard_context kind-patrol- (empty suffix)" -- lib patrol::guard_context kind-patrol-
expect fail "guard_context 'kind-patrol-x;rm' (shell metachar)" -- lib patrol::guard_context "'kind-patrol-x;rm'"
expect fail "guard_context empty" -- lib patrol::guard_context "''"
expect ok "guard_cluster patrol-control" -- lib patrol::guard_cluster patrol-control
expect fail "guard_cluster manager" -- lib patrol::guard_cluster manager
expect fail "guard_cluster patrol (no suffix)" -- lib patrol::guard_cluster patrol
expect ok "patrol::kubectl patrol-control get ns (local envtest)" -- lib patrol::kubectl patrol-control get ns default -o name
expect fail "patrol::kubectl manager (non-patrol cluster)" -- lib patrol::kubectl manager get ns
# A kubeconfig with an allowed context name that points at a non-local server must be refused.
cp "$SIM/cache/patrol-control.kubeconfig" "$SIM/remote.kubeconfig"
kubectl config --kubeconfig "$SIM/remote.kubeconfig" set-cluster kind-patrol-control --server https://203.0.113.10:6443 >/dev/null
expect fail "guard_kubeconfig: kind-patrol-control -> https://203.0.113.10:6443" -- lib patrol::guard_kubeconfig "$SIM/remote.kubeconfig" kind-patrol-control
expect fail "guard_kubeconfig: context missing from file" -- lib patrol::guard_kubeconfig "$SIM/cache/patrol-control.kubeconfig" kind-patrol-target
expect ok "guard_kubeconfig: internal https://patrol-target-control-plane:6443" -- lib patrol::guard_kubeconfig "$SIM/cache/patrol-target-internal.kubeconfig" kind-patrol-target

echo "== PATROL_WAIT_SCALE and patrol::secs"
# secs_is SCALE N WANT: patrol::secs N prints WANT with PATROL_WAIT_SCALE=SCALE ("unset": not set).
secs_is() {
	local got
	if [[ $1 == unset ]]; then
		got=$(env -u PATROL_WAIT_SCALE bash -c "source '$ROOT/hack/kind/lib.sh'; patrol::secs $2") || return 1
	else
		got=$(PATROL_WAIT_SCALE=$1 bash -c "source '$ROOT/hack/kind/lib.sh'; patrol::secs $2") || return 1
	fi
	echo "patrol::secs $2 = $got"
	[[ $got == "$3" ]]
}
# scale_refused VALUE: sourcing lib.sh with PATROL_WAIT_SCALE=VALUE fails with a message naming it.
scale_refused() {
	local out
	if out=$(PATROL_WAIT_SCALE=$1 bash -c "source '$ROOT/hack/kind/lib.sh'; echo sourced" 2>&1); then
		echo "accepted: $out"
		return 1
	fi
	echo "$out"
	[[ $out == *"PATROL_WAIT_SCALE must be an integer from 1 to 10 (got '$1')"* ]]
}
expect ok "patrol::secs 180, PATROL_WAIT_SCALE unset: 180" -- secs_is unset 180 180
expect ok "patrol::secs 180, PATROL_WAIT_SCALE empty (default 1): 180" -- secs_is '' 180 180
expect ok "patrol::secs 180, PATROL_WAIT_SCALE=1: 180" -- secs_is 1 180 180
expect ok "patrol::secs 180, PATROL_WAIT_SCALE=3: 540" -- secs_is 3 180 540
expect ok "patrol::secs 4, PATROL_WAIT_SCALE=10: 40" -- secs_is 10 4 40
for v in 0 11 abc -1 07 2.5; do
	expect ok "PATROL_WAIT_SCALE=$v is refused with a clear message" -- scale_refused "$v"
done
expect fail "hack/kind/env.sh stops when PATROL_WAIT_SCALE=0" -- env PATROL_WAIT_SCALE=0 "$ROOT/hack/kind/env.sh"

echo "== verify-install.sh polling helpers and waits (fake clock)"
# The helpers and the waits are taken from verify-install.sh itself (the script needs the live
# clusters). The clock is fake and deterministic: SECONDS is unset first, which makes it an ordinary
# variable, and sleep is a function that advances it, so nothing here really waits.
sed -n -e '/^until_true() {$/,/^}$/p' -e '/^time_left() /p' "$ROOT/hack/kind/verify-install.sh" >"$SIM/poll.sh"
# The waits of the sync section, as they are in the script: the comparison wait, the sync requests
# and the wait for the operations, up to the Deployment check.
awk '/^# Every Application must have compared once/ {on = 1} /^if deployments=/ {on = 0} on' \
	"$ROOT/hack/kind/verify-install.sh" >"$SIM/loops.sh"
polling_helpers_found() {
	cat "$SIM/poll.sh"
	bash -c 'source "$1" && declare -F until_true time_left' _ "$SIM/poll.sh"
}
# sync_waits_found: the extracted block has both deadlines (150 s and 180 s).
sync_waits_found() {
	cat "$SIM/loops.sh"
	grep -qF 'patrol::secs 150' "$SIM/loops.sh" || return 1
	grep -qF 'patrol::secs 180' "$SIM/loops.sh" || return 1
	echo "the comparison and sync waits: $(grep -c . "$SIM/loops.sh") lines"
}
# until_true_gives LIMIT OK_AT WANT: until_true LIMIT on a probe that succeeds from its OK_AT-th call
# on (0: never) prints WANT, "STATUS CALLS PAUSES ELAPSED" (the pauses are 3 s each; ELAPSED is
# in fake seconds).
until_true_gives() {
	local got
	got=$(bash -c '
		source "$1"
		limit=$2 ok_at=$3 calls=0 pauses=0
		unset SECONDS
		SECONDS=1000
		sleep() {
			pauses=$((pauses + 1))
			SECONDS=$((SECONDS + $1))
		}
		probe() {
			calls=$((calls + 1))
			[[ $ok_at -gt 0 && $calls -ge $ok_at ]]
		}
		if until_true "$limit" probe; then status=0; else status=1; fi
		echo "$status $calls $pauses $((SECONDS - 1000))"
	' _ "$SIM/poll.sh" "$1" "$2") || return 1
	echo "status, probe calls, pauses, fake seconds: $got"
	[[ $got == "$3" ]]
}
# sync_waits_give REQUEST_SECS NEVER_COMPARED NEVER_DONE WANT: runs the extracted waits for the
# Applications a to g at PATROL_WAIT_SCALE 1, with stubs for the helpers they call. The Applications
# in NEVER_COMPARED (space-separated) never compare, the others at their first check; each sync
# request takes REQUEST_SECS; the new operation of an Application not in NEVER_DONE has ended at
# its second check (after one 3 s pause). gate stops the run, like the real one, once anything
# failed. Prints WANT, "FAILED REQUESTS ELAPSED" (FAILED: comma-separated or none; REQUESTS: the
# sync requests sent; ELAPSED: fake seconds). A helper renamed in the script is "not found" here,
# which changes the result.
sync_waits_give() {
	local req=$1 never_compared=$2 never_done=$3 want=$4 got
	rm -rf "$SIM/loops-work"
	mkdir "$SIM/loops-work"
	got=$(bash -c '
		set -uo pipefail
		source "$1"
		work=$3 req=$4 never_compared=" $5 " never_done=" $6 "
		apps=$(printf "%s\n" a b c d e f g) failed="" requests=0
		unset SECONDS
		SECONDS=1000
		sleep() { SECONDS=$((SECONDS + $1)); }
		result() {
			failed=${failed#,}
			echo "${failed:-none} $requests $((SECONDS - 1000))"
		}
		patrol::secs() { echo "$1"; }
		gate() {
			[[ -z $failed ]] && return 0
			result
			exit 0
		}
		# request_sync runs in $(...), where a change of the clock is lost: the time of each request
		# is charged when it is reported.
		ok() {
			case $2 in *": sync request accepted")
				requests=$((requests + 1))
				SECONDS=$((SECONDS + req))
				;;
			esac
		}
		bad() { failed="$failed,${2%%:*}"; }
		app_state() { echo Unknown/Unknown; }
		app_result() { echo Synced/Healthy/Succeeded; }
		app_op() { echo op-1; }
		app_compared() { [[ $never_compared != *" $1 "* ]]; }
		request_sync() {
			app_op "$1" >"$work/op.$1"
			echo 200
		}
		app_done() {
			[[ $never_done != *" $1 "* && -e $work/checked.$1 ]] && return 0
			: >"$work/checked.$1"
			return 1
		}
		source "$2"
		result
	' _ "$SIM/poll.sh" "$SIM/loops.sh" "$SIM/loops-work" "$req" "$never_compared" "$never_done") || return 1
	echo "failed, sync requests, fake seconds: $got"
	[[ $got == "$want" ]]
}
expect ok "until_true and time_left found in verify-install.sh" -- polling_helpers_found
expect ok "comparison and sync waits found in verify-install.sh" -- sync_waits_found
expect ok "until_true 0, probe fails: checked once, no pause, status 1" -- until_true_gives 0 0 "1 1 0 0"
expect ok "until_true 0, probe succeeds: checked once, status 0" -- until_true_gives 0 1 "0 1 0 0"
expect ok "until_true 10, probe succeeds at the 3rd check: after 6 s" -- until_true_gives 10 3 "0 3 2 6"
# Checks at 0, 3, 6 and 9 s, a last pause, and a last check at 12 s.
expect ok "until_true 10, probe never succeeds: gives up at 12 s" -- until_true_gives 10 0 "1 5 4 12"
expect ok "until_true 30, probe never succeeds: gives up at 30 s" -- until_true_gives 30 0 "1 11 10 30"
expect ok "waits: all compare, operations end at the 2nd check: 21 s" -- sync_waits_give 0 "" "" "none 7 21"
# a uses up the one 150 s comparison deadline; b to f still get their one check and pass; g fails at
# once. The gate then stops the run before any sync request.
expect ok "waits: a and g never compare: they fail at 150 s, no request" -- sync_waits_give 0 "a g" "" "a,g 0 150"
# Seven requests of 30 s each (210 s); the 180 s deadline of the operations counts from the last.
expect ok "waits: 30 s per request: sync deadline counts from the last" -- sync_waits_give 30 "" "" "none 7 231"
# a to f end at their 2nd check (18 s); g, whose operation never ends, gets the rest of the one
# 180 s deadline.
expect ok "waits: g's operation never ends: only g fails, at 180 s" -- sync_waits_give 0 "" g "g 7 180"

echo "== digest-pinned images"
# pinned REF: REF is NAME[:TAG]@sha256:<64 hex digits>.
pinned() {
	echo "$1"
	[[ $1 =~ ^[^@]+@sha256:[0-9a-f]{64}$ ]]
}
default_of() { env -u "$1" bash -c "source '$ROOT/hack/kind/lib.sh'; printf '%s\n' \"\$$1\""; }
expect ok "default PATROL_REGISTRY_IMAGE is digest-pinned" -- pinned "$(default_of PATROL_REGISTRY_IMAGE)"
expect ok "default PATROL_KIND_NODE_IMAGE is digest-pinned" -- pinned "$(default_of PATROL_KIND_NODE_IMAGE)"
expect ok "toolbox Dockerfile: the oras stage is digest-pinned" -- \
	pinned "$(sed -n 's/^FROM \([^ ]*\) AS oras$/\1/p' "$ROOT/test/e2e/fixtures/toolbox/Dockerfile")"

echo "== patrol::docker_pull_retry and the load-image.sh fallback (fake docker, kind, sleep)"
fakebin=$SIM/fakebin
mkdir -p "$fakebin"
cat >"$fakebin/docker" <<'EOF'
#!/bin/sh
# Fake docker: records every call. "pull" fails the first $FAKE_PULL_FAILS times; "save" writes a
# partial archive and then fails when FAKE_SAVE_FAILS=1.
printf '%s\n' "$*" >>"$FAKE_STATE/docker.calls"
case $1 in
pull)
	if [ "$(grep -c '^pull ' "$FAKE_STATE/docker.calls")" -le "${FAKE_PULL_FAILS:-0}" ]; then
		echo "Error response from daemon: toomanyrequests: fake rate limit" >&2
		exit 1
	fi
	;;
version) echo arm64 ;;
save)
	while [ $# -gt 1 ]; do
		if [ "$1" = -o ]; then echo partial >"$2"; fi
		shift
	done
	if [ "${FAKE_SAVE_FAILS:-0}" = 1 ]; then
		echo "fake docker save failure" >&2
		exit 1
	fi
	;;
esac
exit 0
EOF
cat >"$fakebin/kind" <<'EOF'
#!/bin/sh
# Fake kind: both patrol clusters exist; "load docker-image" always fails.
printf '%s\n' "$*" >>"$FAKE_STATE/kind.calls"
case "$1 $2" in
"get clusters") printf 'patrol-control\npatrol-target\n' ;;
"load docker-image")
	echo "ERROR: fake kind load docker-image failure" >&2
	exit 1
	;;
esac
exit 0
EOF
cat >"$fakebin/sleep" <<'EOF'
#!/bin/sh
# Fake sleep: records the delay instead of sleeping.
printf '%s\n' "$*" >>"$FAKE_STATE/sleep.calls"
EOF
chmod 755 "$fakebin/docker" "$fakebin/kind" "$fakebin/sleep"
# with_fakes CMD...: runs CMD with the fakes first on PATH and a fresh call record.
with_fakes() {
	rm -rf "$SIM/fake-state"
	mkdir -p "$SIM/fake-state"
	PATH="$fakebin:$PATH" FAKE_STATE=$SIM/fake-state "$@"
}
# pull_failing N: patrol::docker_pull_retry against a docker whose first N pulls fail.
pull_failing() {
	with_fakes env FAKE_PULL_FAILS="$1" bash -c "source '$ROOT/hack/kind/lib.sh'; patrol::docker_pull_retry example.test/fake:1"
}
# pull_record ATTEMPTS SLEEPS: the last pull_failing made ATTEMPTS identical pull calls and slept
# SLEEPS (comma-separated seconds).
pull_record() {
	local calls sleeps
	calls=$(grep -c . "$SIM/fake-state/docker.calls")
	sleeps=$(paste -s -d , "$SIM/fake-state/sleep.calls" 2>/dev/null) || sleeps=""
	echo "pull calls: $calls, sleeps: ${sleeps:-none}"
	[[ $calls == "$1" && $(grep -cx 'pull --quiet example.test/fake:1' "$SIM/fake-state/docker.calls") == "$1" && $sleeps == "$2" ]]
}
pull_gives_up() {
	local out
	if out=$(pull_failing 99 2>&1); then
		echo "the pull succeeded"
		return 1
	fi
	printf '%s\n' "$out"
	[[ $out == *"docker pull example.test/fake:1 failed after 3 attempts"* ]]
}
expect ok "docker_pull_retry: a working pull" -- pull_failing 0
expect ok "  ... is tried once, without sleeping" -- pull_record 1 ""
expect ok "docker_pull_retry: a pull that fails twice succeeds" -- pull_failing 2
expect ok "  ... on the 3rd attempt, after 5 s and 15 s" -- pull_record 3 "5,15"
expect ok "docker_pull_retry: a pull that always fails stops, naming the image" -- pull_gives_up
expect ok "  ... after 3 attempts (5 s and 15 s apart)" -- pull_record 3 "5,15"
# load-image.sh: kind load docker-image fails, so the docker save fallback runs.
load_fallback() { with_fakes env FAKE_SAVE_FAILS="$1" "$ROOT/hack/kind/load-image.sh" --cluster patrol-control patrol-cd/fake:dev; }
load_fallback_logged() {
	local out
	out=$(load_fallback 0 2>&1) || {
		printf '%s\n' "$out"
		return 1
	}
	printf '%s\n' "$out"
	[[ $out == *"loaded patrol-cd/fake:dev into patrol-control (docker save --platform linux/arm64 + kind load image-archive)"* ]]
}
# no_temp_left: the directory of the archive the fake docker save wrote (-o) was removed.
no_temp_left() {
	local archive
	archive=$(sed -n 's/^save .* -o \([^ ]*\) .*/\1/p' "$SIM/fake-state/docker.calls")
	[[ -n $archive ]] || {
		echo "docker save was not called"
		return 1
	}
	echo "archive directory $(dirname "$archive")"
	[[ ! -e $(dirname "$archive") ]]
}
expect ok "load-image.sh: fallback to docker save + kind load image-archive, logged" -- load_fallback_logged
expect ok "  ... leaves no temporary directory" -- no_temp_left
expect fail "load-image.sh: the fallback's docker save fails" -- load_fallback 1
expect ok "  ... and still leaves no temporary directory" -- no_temp_left

echo "== register-target.sh on a control plane without CRDs"
expect fail "register-target.sh without CRDs and without --install-crds" -- "$ROOT/hack/kind/register-target.sh" --namespace patrol-system
expect ok "register-target.sh --install-crds (fresh)" -- "$ROOT/hack/kind/register-target.sh" --namespace patrol-system --install-crds

echo "== CEL rejection sanity check (CRDs enforce rules in this setup)"
# CRDs were installed by register-target.sh --install-crds above.
cat >"$SIM/bad-app.yaml" <<'EOF'
apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata: {name: bad, namespace: default}
spec:
  project: default
  source: {repoURL: http://git-server.patrol-e2e.svc.cluster.local/git/guestbook.git, path: .}
  sources: [{repoURL: http://git-server.patrol-e2e.svc.cluster.local/git/guestbook.git, path: .}]
  destination: {name: patrol-target, namespace: x}
EOF
expect fail "Application with source and sources is rejected" -- \
	kubectl --kubeconfig "$SIM/cache/patrol-control.kubeconfig" --context kind-patrol-control create --dry-run=server -f "$SIM/bad-app.yaml"

echo "== register-target.sh"
expect fail "register-target.sh without --namespace" -- "$ROOT/hack/kind/register-target.sh"
expect ok "register-target.sh --namespace patrol-system --install-crds" -- "$ROOT/hack/kind/register-target.sh" --namespace patrol-system --install-crds
expect ok "register-target.sh again (idempotent)" -- "$ROOT/hack/kind/register-target.sh" --namespace patrol-system
C() { kubectl --kubeconfig "$SIM/cache/patrol-control.kubeconfig" --context kind-patrol-control "$@"; }
T() { kubectl --kubeconfig "$SIM/cache/patrol-target.kubeconfig" --context kind-patrol-target "$@"; }
expect ok "Cluster patrol-system/patrol-target server" -- \
	bash -c "test \"\$(kubectl --kubeconfig '$SIM/cache/patrol-control.kubeconfig' --context kind-patrol-control -n patrol-system get clusters.cd.patrol.example patrol-target -o jsonpath='{.spec.server} {.spec.config.credentialsRef.name}')\" = 'https://patrol-target-control-plane:6443 cluster-patrol-target' && echo match"
C -n patrol-system get secret cluster-patrol-target -o go-template='{{index .data "bearerToken" | base64decode}}' >"$SIM/token"
C -n patrol-system get secret cluster-patrol-target -o go-template='{{index .data "caData" | base64decode}}' >"$SIM/ca.pem"
expect ok "Secret keys are exactly bearerToken,caData" -- \
	bash -c "test \"\$(kubectl --kubeconfig '$SIM/cache/patrol-control.kubeconfig' --context kind-patrol-control -n patrol-system get secret cluster-patrol-target -o go-template='{{range \$k, \$v := .data}}{{\$k}},{{end}}')\" = 'bearerToken,caData,' && echo match"
expect ok "Secret has no last-applied annotation" -- \
	bash -c "! kubectl --kubeconfig '$SIM/cache/patrol-control.kubeconfig' --context kind-patrol-control -n patrol-system get secret cluster-patrol-target -o yaml | grep -q last-applied && echo none"
C -n patrol-system get secret cluster-patrol-target -o go-template='{{index .metadata.labels "cd.patrol.example/secret-type"}}' >"$SIM/label"
expect ok "Secret is labeled cd.patrol.example/secret-type=cluster" -- grep -x cluster "$SIM/label"
expect ok "caData equals patrol-target's serving CA" -- cmp "$SIM/ca.pem" "$SIM/target/certs/apiserver.crt"
# Use the stored credentials against the target API server, verifying TLS with caData.
cat >"$SIM/patrol-manager.kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters: [{name: t, cluster: {server: "$TARGET_SERVER", certificate-authority: "$SIM/ca.pem"}}]
users: [{name: m, user: {tokenFile: "$SIM/token"}}]
contexts: [{name: m, context: {cluster: t, user: m}}]
current-context: m
EOF
expect ok "token authenticates as kube-system:patrol-manager (TLS verified)" -- \
	bash -c "test \"\$(kubectl --kubeconfig '$SIM/patrol-manager.kubeconfig' auth whoami -o jsonpath='{.status.userInfo.username}')\" = system:serviceaccount:kube-system:patrol-manager && echo match"
expect ok "token is cluster-admin on target" -- kubectl --kubeconfig "$SIM/patrol-manager.kubeconfig" auth can-i '*' '*' --all-namespaces

echo "== test/e2e/fixtures/patrol/register.sh"
C create namespace patrol-e2e >/dev/null
printf 'patrol-e2e' >"$SIM/u"
openssl rand -hex 24 | tr -d '\n' >"$SIM/p"
ssh-keygen -q -t ed25519 -N '' -C sim -f "$SIM/id"
C -n patrol-e2e create secret generic git-server-http-auth --from-file=username="$SIM/u" --from-file=password="$SIM/p" >/dev/null
C -n patrol-e2e create secret generic chart-repo-auth --from-file=username="$SIM/u" --from-file=password="$SIM/p" >/dev/null
C -n patrol-e2e create secret generic git-server-ssh-client --from-file=sshPrivateKey="$SIM/id" --from-file=known_hosts="$ROOT/test/e2e/fixtures/git-server/known_hosts" >/dev/null
expect fail "register.sh with invalid namespace" -- "$ROOT/test/e2e/fixtures/patrol/register.sh" --namespace Bad_NS
# The known-hosts ConfigMap already exists with an unrelated entry (no trailing newline) and no
# label: register.sh must keep the entry, add the fixture lines and label the ConfigMap.
printf 'other.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOTHERHOSTKEY' >"$SIM/other_host"
C -n patrol-system create configmap patrol-ssh-known-hosts-cm --from-file=ssh_known_hosts="$SIM/other_host" >/dev/null
# A ConfigMap that has data but not the key must not turn into a "<no value>" line.
C create namespace patrol-nokey >/dev/null
C -n patrol-nokey create configmap patrol-ssh-known-hosts-cm --from-literal=unrelated=x >/dev/null
expect ok "register.sh --namespace patrol-nokey (ConfigMap without the key)" -- "$ROOT/test/e2e/fixtures/patrol/register.sh" --namespace patrol-nokey
C -n patrol-nokey get configmap patrol-ssh-known-hosts-cm -o go-template='{{index .data "ssh_known_hosts"}}' >"$SIM/known_hosts_nokey"
grep -v '^#' "$ROOT/test/e2e/fixtures/git-server/known_hosts" | grep . >"$SIM/known_hosts_fixture"
expect ok "a ConfigMap without the key gets exactly the fixture lines" -- cmp "$SIM/known_hosts_nokey" "$SIM/known_hosts_fixture"
expect ok "register.sh --namespace patrol-system --with-applications" -- "$ROOT/test/e2e/fixtures/patrol/register.sh" --namespace patrol-system --with-applications
expect ok "register.sh again (idempotent)" -- "$ROOT/test/e2e/fixtures/patrol/register.sh" --namespace patrol-system --with-applications
C -n patrol-system get configmap patrol-ssh-known-hosts-cm -o go-template='{{index .data "ssh_known_hosts"}}' >"$SIM/known_hosts"
expect ok "known hosts keep the entry that was there" -- grep -xF "$(cat "$SIM/other_host")" "$SIM/known_hosts"
grep -vxF "$(cat "$SIM/other_host")" "$SIM/known_hosts" >"$SIM/known_hosts_added"
expect ok "known hosts gained exactly the fixture lines (4 names, once each)" -- cmp "$SIM/known_hosts_added" "$SIM/known_hosts_fixture"
expect ok "known hosts ConfigMap is labeled part-of=patrol-cd" -- test "$(C -n patrol-system get configmap patrol-ssh-known-hosts-cm -o go-template='{{index .metadata.labels "app.kubernetes.io/part-of"}}')" = patrol-cd
expect ok "objects present" -- C -n patrol-system get repositories.cd.patrol.example,repositorycredentials.cd.patrol.example,applications.cd.patrol.example -o name
C -n patrol-system get repositories.cd.patrol.example,repositorycredentials.cd.patrol.example,applications.cd.patrol.example,clusters.cd.patrol.example -o name | sed 's/^/      /'
C -n patrol-system get secret e2e-git-http-creds -o go-template='{{index .data "password" | base64decode}}' >"$SIM/p2"
expect ok "e2e-git-http-creds password copied byte-for-byte" -- cmp "$SIM/p" "$SIM/p2"
C -n patrol-system get secret e2e-git-ssh-creds -o go-template='{{index .data "sshPrivateKey" | base64decode}}' >"$SIM/id2"
expect ok "e2e-git-ssh-creds sshPrivateKey copied byte-for-byte" -- cmp "$SIM/id" "$SIM/id2"
for want in e2e-git-http-creds:repo-creds e2e-git-ssh-creds:repo-creds e2e-chart-repo-creds:repository; do
	C -n patrol-system get secret "${want%%:*}" -o go-template='{{index .metadata.labels "cd.patrol.example/secret-type"}}' >"$SIM/label"
	expect ok "Secret ${want%%:*} is labeled secret-type=${want##*:}" -- grep -x "${want##*:}" "$SIM/label"
done

echo "== restore-drill.sh checks before the namespace deletion"
# The functions are taken from restore-drill.sh itself (the script needs the live clusters) and run
# against namespace patrol-drill of the control envtest and the e2e-* namespaces of the target one.
sed -n -e '/^patrol_kinds=/p' -e '/^listing() {$/,/^}$/p' -e '/^finalizers_left() {$/,/^}$/p' \
	-e '/^strip_finalizers() {$/,/^}$/p' -e '/^active_operations() {$/,/^}$/p' -e '/^controller_pods() {$/,/^}$/p' \
	-e '/^target_state() {$/,/^}$/p' -e '/^backup_names() /p' \
	"$ROOT/hack/kind/restore-drill.sh" >"$SIM/drill.sh"
drill_helpers_found() {
	cat "$SIM/drill.sh"
	bash -c 'source "$1" && declare -F listing finalizers_left strip_finalizers active_operations controller_pods target_state backup_names' _ "$SIM/drill.sh"
}
# drill FUNCTION ARGS...: runs a function of the drill with its k and kt wrappers. While
# drill_api_down is 1, k fails as kubectl does when the API server does not answer.
drill_api_down=0
drill() {
	bash -c '
		set -uo pipefail
		source "$1/hack/kind/lib.sh"
		source "$2"
		work=$3
		k() { patrol::kubectl patrol-control -n patrol-drill "$@"; }
		if [[ $4 == 1 ]]; then
			k() {
				echo "the server was unable to return a response in the time allotted" >&2
				return 1
			}
		fi
		kt() { patrol::kubectl patrol-target "$@"; }
		shift 4
		"$@"
	' _ "$ROOT" "$SIM/drill.sh" "$SIM" "$drill_api_down" "$@"
}
# drill_prints WANT FUNCTION: FUNCTION succeeds and prints exactly WANT (lines joined by spaces).
drill_prints() {
	local want=$1 got
	shift
	got=$(drill "$@" | tr '\n' ' ') || return 1
	echo "printed: '$got'"
	[[ $got == "$want" ]]
}
# drill_down FUNCTION and down_prints WANT FUNCTION: the same with the API server down.
drill_down() {
	local drill_api_down=1
	drill "$@"
}
down_prints() {
	local drill_api_down=1
	drill_prints "$@"
}
C create namespace patrol-drill >/dev/null
# Every kind in patrol_kinds carries a finalizer; strip_finalizers may only remove those of the
# Applications, ApplicationSets and AppProjects.
cat <<'EOF' | C -n patrol-drill create -f - >/dev/null
apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata: {name: kept, finalizers: [resources-finalizer.cd.patrol.example]}
spec:
  project: default
  source: {repoURL: http://git.example/r.git, path: .}
  destination: {name: patrol-target, namespace: x}
---
apiVersion: cd.patrol.example/v1alpha1
kind: Application
metadata: {name: plain}
spec:
  project: default
  source: {repoURL: http://git.example/r.git, path: .}
  destination: {name: patrol-target, namespace: x}
---
apiVersion: cd.patrol.example/v1alpha1
kind: ApplicationSet
metadata: {name: set, finalizers: [resources-finalizer.cd.patrol.example]}
spec:
  generators: [{list: {elements: []}}]
  template:
    metadata: {name: x}
    spec:
      project: default
      source: {repoURL: http://git.example/r.git, path: .}
      destination: {name: patrol-target, namespace: x}
---
apiVersion: cd.patrol.example/v1alpha1
kind: AppProject
metadata: {name: proj, finalizers: [resources-finalizer.cd.patrol.example]}
spec: {}
---
apiVersion: cd.patrol.example/v1alpha1
kind: Operation
metadata: {name: running, finalizers: [example.com/hold]}
spec: {application: kept, sync: {}}
---
apiVersion: cd.patrol.example/v1alpha1
kind: Operation
metadata: {name: unstarted}
spec: {application: plain, sync: {}}
---
apiVersion: cd.patrol.example/v1alpha1
kind: Cluster
metadata: {name: c, finalizers: [example.com/hold]}
spec: {name: c, server: https://c.example}
---
apiVersion: cd.patrol.example/v1alpha1
kind: Repository
metadata: {name: r, finalizers: [example.com/hold]}
spec: {url: http://git.example/r.git}
---
apiVersion: cd.patrol.example/v1alpha1
kind: RepositoryCredential
metadata: {name: rc, finalizers: [example.com/hold]}
spec: {urlPrefix: http://git.example/}
---
apiVersion: cd.patrol.example/v1alpha1
kind: SCMProvider
metadata: {name: s, finalizers: [example.com/hold]}
spec: {type: github}
EOF
C -n patrol-drill patch operations.cd.patrol.example running --subresource=status --type=merge -p '{"status":{"phase":"Running"}}' >/dev/null
# One Operation in every other phase of the CRD's enum, named after it.
for phase in Pending Terminating Succeeded Failed Error Cancelled Rejected; do
	op=$(tr '[:upper:]' '[:lower:]' <<<"$phase")
	printf 'apiVersion: cd.patrol.example/v1alpha1\nkind: Operation\nmetadata: {name: %s}\nspec: {application: plain, sync: {}}\n' "$op" |
		C -n patrol-drill create -f - >/dev/null
	C -n patrol-drill patch operations.cd.patrol.example "$op" --subresource=status --type=merge -p "{\"status\":{\"phase\":\"$phase\"}}" >/dev/null
done
C -n patrol-drill create serviceaccount default >/dev/null
for pod in patrol-controller-0:patrol-controller patrol-applicationset-controller-a:patrol-applicationset-controller \
	patrol-server-a:patrol-server; do
	C -n patrol-drill run "${pod%%:*}" --image=registry.example/patrol:1 --labels="app.kubernetes.io/name=${pod##*:}" >/dev/null
done
T create namespace e2e-drill >/dev/null
T create namespace other-drill >/dev/null
T -n e2e-drill create serviceaccount default >/dev/null
T -n e2e-drill create service clusterip web --tcp=80:8080 >/dev/null
T -n e2e-drill create deployment web --image=registry.example/web:1 >/dev/null
cat <<'EOF' | T -n e2e-drill create -f - >/dev/null
apiVersion: apps/v1
kind: ReplicaSet
metadata: {name: web-1}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec: {containers: [{name: web, image: registry.example/web:1}]}
EOF
T -n e2e-drill run web-1-a --image=registry.example/web:1 >/dev/null
T -n other-drill create service clusterip web --tcp=80:8080 >/dev/null
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: present}\n' >"$SIM/backup-present.yaml"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata: {name: present}\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: absent}\n' >"$SIM/backup-absent.yaml"
C -n patrol-drill create configmap present >/dev/null
expect ok "listing, finalizers_left, strip_finalizers, active_operations, controller_pods, target_state, backup_names found" -- drill_helpers_found
held='["example.com/hold"]'
rf='["resources-finalizer.cd.patrol.example"]'
expect ok "finalizers_left lists an object of every kind in patrol_kinds" -- \
	drill_prints "Application/kept $rf ApplicationSet/set $rf AppProject/proj $rf Operation/running $held Cluster/c $held Repository/r $held RepositoryCredential/rc $held SCMProvider/s $held " finalizers_left
expect ok "active_operations lists the Pending, Running, Terminating and phaseless Operations only" -- \
	drill_prints 'pending Pending running Running terminating Terminating unstarted  ' active_operations
expect ok "controller_pods lists the application and ApplicationSet controller pods only" -- \
	drill_prints 'pod/patrol-applicationset-controller-a pod/patrol-controller-0 ' controller_pods
expect ok "strip_finalizers" -- drill strip_finalizers
# The drill stops when anything is left: strip_finalizers leaves the other kinds alone.
expect ok "after strip_finalizers only the finalizers of the other kinds are left" -- \
	drill_prints "Operation/running $held Cluster/c $held Repository/r $held RepositoryCredential/rc $held SCMProvider/s $held " finalizers_left
for o in operations/running clusters/c repositories/r repositorycredentials/rc scmproviders/s; do
	C -n patrol-drill patch "${o%%/*}.cd.patrol.example" "${o##*/}" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null
done
expect ok "finalizers_left prints nothing once none is left" -- drill_prints '' finalizers_left
# The drill's checks compare listing's output with "": a list that cannot be read must not pass.
expect ok "listing joins the lines a command prints" -- drill_prints 'a b' listing printf 'a\nb\n'
expect ok "listing prints nothing for an empty list" -- drill_prints '' listing true
for f in finalizers_left strip_finalizers active_operations controller_pods; do
	expect fail "$f fails when the API server does not answer" -- drill_down "$f"
done
expect ok "listing reports the error when the API server does not answer" -- \
	down_prints 'error: the server was unable to return a response in the time allotted ' listing finalizers_left
# target_state must print the UID and the generation (empty for kinds without one) of each object.
uid() { T -n e2e-drill get "$@" -o jsonpath='{.metadata.uid}'; }
{
	echo "Deployment e2e-drill/web $(uid deployment web) 1"
	echo "Namespace /e2e-drill $(T get namespace e2e-drill -o jsonpath='{.metadata.uid}') "
	echo "Pod e2e-drill/web-1-a $(uid pod web-1-a) $(T -n e2e-drill get pod web-1-a -o jsonpath='{.metadata.generation}')"
	echo "ReplicaSet e2e-drill/web-1 $(uid replicaset web-1) 1"
	echo "Service e2e-drill/web $(uid service web) "
} >"$SIM/target-want"
target_lists() {
	drill target_state >"$SIM/target-got" || return 1
	diff "$SIM/target-want" "$SIM/target-got"
}
expect ok "target_state lists the e2e-* Namespace, Deployment, ReplicaSet, Pod and Service with UID and generation, nothing else" -- \
	target_lists
expect ok "backup_names prints the objects of a backup" -- drill_prints 'configmap/present ' backup_names "$SIM/backup-present.yaml"
expect fail "backup_names fails when an object of the backup is missing" -- drill backup_names "$SIM/backup-absent.yaml"

echo "== env.sh"
expect ok "env.sh output evaluates" -- bash -c "eval \"\$('$ROOT/hack/kind/env.sh' 2>/dev/null)\" && test \"\$PATROL_TARGET_INTERNAL_SERVER\" = https://patrol-target-control-plane:6443 && echo \"\$PATROL_CONTROL_KUBECONFIG\""

echo "test-scripts: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
