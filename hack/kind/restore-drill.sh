#!/usr/bin/env bash
# Backup and restore drill of Patrol CD on the kind clusters (make kind-restore-drill). It backs the
# installation up with `patrol admin export`, loses it, installs the same chart again with the same
# values and imports the backup, then checks that the Applications come back Synced/Healthy with
# their credentials and that nothing they deployed on patrol-target was deleted or recreated. Run it
# after hack/kind/install.sh and hack/kind/verify-install.sh (which syncs the fixture Applications).
#
# Usage: hack/kind/restore-drill.sh [--namespace NS] [--new-cluster]
#
# What is lost:
#   default        the namespace NS. The drill removes it the way docs/guides/backup-restore.md
#                  removes an installation without touching what it deployed: it stops the
#                  application controller and the ApplicationSet controller, removes the finalizers
#                  of the Applications, ApplicationSets and AppProjects, and deletes the namespace.
#   --new-cluster  the whole patrol-control cluster, deleted and created again (hack/kind/up.sh),
#                  with the fixtures deployed again (test/e2e/fixtures/deploy.sh). The credentials
#                  of the fixture git server and chart repository are carried over to the new
#                  cluster first: they stand for external systems, which keep their credentials
#                  when a Patrol CD cluster is lost. patrol-target and patrol-registry stay.
#
# Steps:
#   1. preconditions (nothing is changed before they all pass): Patrol CD installed and healthy in
#      NS, the 7 fixture Applications Synced/Healthy and their 7 Deployments available on
#      patrol-target, the password in patrol-initial-admin-secret accepted. The drill then adds
#      resources-finalizer.cd.patrol.example to e2e-guestbook: a procedure that let the controller
#      delete that Application would delete its workload on patrol-target.
#   2. backup: `patrol admin export -n NS` run with the patrol CLI of the installed image (docker
#      run on the kind network, as the guide runs the CLI of the installed version), the Helm
#      values (`helm get values`) and the chart version. The backup holds Secrets: it stays in a
#      mode-0700 temporary directory, removed on exit, and is never printed.
#   3. the loss (above); 4. `helm install` of the same chart with the saved values;
#   5. `patrol admin import -n NS -`, once the new installation has created its default AppProject.
#
# Checks after the import: the import exits 0 with no warning; every object of the backup exists
# again; the admin password from before the drill logs in and the password the new installation
# generated is refused (patrol-secret was restored); the 7 Applications reach Synced/Healthy from
# their empty restored status (the SSH and HTTP git credentials, the restored known hosts and the
# patrol-target cluster credential all work) with no Operation created; the private Helm
# repository's restored credential passes the API's connection check, and the repository refuses
# anonymous requests; e2e-guestbook has its finalizer again; the UIDs and generations of the
# Namespaces, Deployments, ReplicaSets, Pods and Services in the e2e-* namespaces on patrol-target
# are unchanged.
#
# The environment is left usable for the other kind scripts: the drill's finalizer is removed again
# and, because they read the admin password from patrol-initial-admin-secret, that Secret is given
# the restored password (the guide says to delete it after a restore; it is stale). When a step
# after the loss fails, the run stops and prints the commands that put the environment back.
#
# Waits are multiplied by PATROL_WAIT_SCALE (hack/kind/lib.sh). The API address is
# https://127.0.0.1:8443; PATROL_API may set another port on 127.0.0.1, localhost or [::1].
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=hack/kind/lib.sh
source "$here/lib.sh"

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "${BASH_SOURCE[0]}"; }

ns=patrol
new_cluster=0
while [[ $# -gt 0 ]]; do
	case "$1" in
	--namespace)
		[[ $# -ge 2 ]] || patrol::die "--namespace needs a value"
		ns=$2
		shift 2
		;;
	--new-cluster)
		new_cluster=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) patrol::die "unexpected argument: $1 (see --help)" ;;
	esac
done
[[ $ns =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]] || patrol::die "invalid namespace name: $ns"

patrol::require_tools docker kind kubectl helm curl
control=$PATROL_CONTROL_CLUSTER
target=$PATROL_TARGET_CLUSTER
patrol::cluster_exists "$control" || patrol::die "kind cluster $control does not exist (run hack/kind/up.sh)"
patrol::cluster_exists "$target" || patrol::die "kind cluster $target does not exist (run hack/kind/up.sh)"
api=${PATROL_API:-https://127.0.0.1:8443}
[[ $api =~ ^https://(127\.0\.0\.1|localhost|\[::1\])(:[0-9]{1,5})?$ ]] ||
	patrol::die "PATROL_API must be an https address on 127.0.0.1, localhost or [::1] (got '$api')"

# The work directory holds the backup, the saved values, the admin passwords and a kubeconfig.
work=$(mktemp -d)
chmod 700 "$work"
# dirty is set while the environment needs a person's attention; the trap says so on every way out.
dirty=""
finish() {
	rm -rf "$work"
	[[ -z $dirty ]] || echo "restore-drill: WARNING: $dirty" >&2
}
trap finish EXIT

pass=0
fail=0
ok() {
	pass=$((pass + 1))
	printf 'PASS  %-8s %s\n' "$1" "$2"
}
bad() {
	fail=$((fail + 1))
	printf 'FAIL  %-8s %s\n' "$1" "$2"
}
# eq AREA DESCRIPTION ACTUAL WANT
eq() {
	if [[ "$3" == "$4" ]]; then ok "$1" "$2"; else bad "$1" "$2: got '$3', want '$4'"; fi
}
# until_true SECONDS CMD... and time_left DEADLINE: as in hack/kind/verify-install.sh. CMD runs at
# least once, also with a limit of 0, and once more after the last pause.
until_true() {
	local end=$((SECONDS + $1))
	shift
	while :; do
		"$@" >/dev/null 2>&1 && return 0
		((SECONDS < end)) || return 1
		sleep 3
	done
}
time_left() { echo $(($1 > SECONDS ? $1 - SECONDS : 0)); }
# gate WHAT: the run stops here, before WHAT, unless every check so far passed.
gate() {
	[[ $fail -eq 0 ]] && return 0
	echo
	echo "restore-drill: earlier checks failed; not running $1" >&2
	echo "restore-drill: $pass passed, $fail failed"
	exit 1
}
# stop REASON: a step after the loss failed; the environment cannot be trusted past this point.
stop() {
	echo
	echo "restore-drill: stopped: $1" >&2
	echo "restore-drill: $pass passed, $fail failed"
	exit 1
}

k() { patrol::kubectl "$control" -n "$ns" "$@"; }
kt() { patrol::kubectl "$target" "$@"; }
helmc() { patrol::helm "$control" -n "$ns" "$@"; }

# The Patrol CD kinds that can carry finalizers in the namespace.
patrol_kinds=applications.cd.patrol.example,applicationsets.cd.patrol.example,appprojects.cd.patrol.example,operations.cd.patrol.example,clusters.cd.patrol.example,repositories.cd.patrol.example,repositorycredentials.cd.patrol.example,scmproviders.cd.patrol.example
# listing CMD...: the lines CMD prints, joined by spaces, or "error: <its last error line>" when it
# fails. A check that wants an empty list therefore fails when the list cannot be read.
listing() {
	local out
	out=$("$@" 2>"$work/listing.err") || {
		echo "error: $(tail -n 1 "$work/listing.err")"
		return 0
	}
	printf '%s' "$out" | tr '\n' ' '
}
# The helpers below fail when kubectl fails (pipefail), so listing reports it.
# finalizers_left: prints the Patrol CD objects in the namespace that still have finalizers.
finalizers_left() {
	k get "$patrol_kinds" -o jsonpath='{range .items[*]}{.kind}{"/"}{.metadata.name}{" "}{.metadata.finalizers}{"\n"}{end}' |
		awk 'NF > 1'
}
# strip_finalizers: removes the finalizers of every Application, ApplicationSet and AppProject in
# the namespace. Only safe once the controllers are stopped: they would add them again.
strip_finalizers() {
	local o names
	names=$(k get applications.cd.patrol.example,applicationsets.cd.patrol.example,appprojects.cd.patrol.example -o name) || return 1
	for o in $names; do
		k patch "$o" --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null || return 1
	done
}
# active_operations: prints the Operations whose phase is not final (a sync in progress or about
# to start). The final phases are those OperationPhase.IsTerminal names (api/v1alpha1/helpers.go);
# any other phase, or none yet, counts as in progress.
active_operations() {
	k get operations.cd.patrol.example -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.phase}{"\n"}{end}' |
		awk '$2 !~ /^(Succeeded|Failed|Error|Cancelled|Rejected)$/'
}
# controller_pods: prints the pods of the application controller and the ApplicationSet controller.
controller_pods() {
	k get pods -l 'app.kubernetes.io/name in (patrol-controller,patrol-applicationset-controller)' -o name
}
# patrol_crds: prints the Patrol CD CRDs of the control-plane cluster.
patrol_crds() { patrol::kubectl "$control" get crd -o name | awk '/\.cd\.patrol\.example$/'; }
# target_state: kind, namespace/name, UID and generation of the Namespaces, Deployments,
# ReplicaSets, Pods and Services of the e2e-* namespaces on patrol-target, sorted.
target_state() {
	kt get namespaces,deployments,replicasets,pods,services -A \
		-o jsonpath='{range .items[*]}{.kind}{" "}{.metadata.namespace}{"/"}{.metadata.name}{" "}{.metadata.uid}{" "}{.metadata.generation}{"\n"}{end}' |
		awk '$2 ~ /^\/?e2e-/' | sort
}
# backup_names FILE: the objects of an export, as `kubectl get -o name` prints them (sorted). Fails
# when one of them does not exist in the namespace.
backup_names() { k get -f "$1" -o name | sort; }
app_state() { k get applications.cd.patrol.example "$1" -o jsonpath='{.status.sync.status}/{.status.health.status}'; }
app_synced_healthy() { [[ "$(app_state "$1")" == Synced/Healthy ]]; }

# --- API helpers: passwords travel through files and pipes only, never argv or output.
secret_value() { k get secret "$1" -o jsonpath="{.data.$2}" | base64 -d; }
# login PASSWORD_FILE [TOKEN_OUT]: prints the HTTP status.
login() {
	local pwfile=$1 out=${2:-/dev/null}
	{
		printf '{"username":"admin","password":"'
		cat "$pwfile"
		printf '"}'
	} | curl -sk -m 20 -X POST "$api/api/v1/session" -H 'Content-Type: application/json' --data-binary @- -o "$out" -w '%{http_code}'
}
# auth_header TOKEN_JSON HEADER_FILE
auth_header() { sed -n 's/.*"token":"\([^"]*\)".*/Authorization: Bearer \1/p' "$1" >"$2"; }
healthz_ok() { [[ "$(curl -sk -m 5 -o /dev/null -w '%{http_code}' "$api/healthz")" == 200 ]]; }

# --- The patrol CLI of the installed image, run on the kind network with a kubeconfig whose server
# is the control-plane container (the host's 127.0.0.1 address is not reachable from a container).
write_cli_kubeconfig() {
	kind get kubeconfig --name "$control" --internal >"$work/kubeconfig" || return 1
	chmod 600 "$work/kubeconfig"
	patrol::guard_kubeconfig "$work/kubeconfig" "kind-$control"
}
patrol_cli() {
	docker run --rm -i --network "$PATROL_KIND_NETWORK" --user "$(id -u):$(id -g)" -e HOME=/tmp \
		-v "$work/kubeconfig:/tmp/kubeconfig:ro" "$cli_image" \
		patrol "$@" --kubeconfig /tmp/kubeconfig --context "kind-$control"
}

echo "== preconditions ($control/$ns)"
helmc status patrol >/dev/null 2>&1 || patrol::die "Patrol CD (release patrol) is not installed in $control/$ns (run make kind-install)"
notready=$(k get pods -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.phase}{" "}{.status.containerStatuses[*].ready}{"\n"}{end}' |
	awk '$2 != "Running" || $3 !~ /^true( true)*$/ {print $1}')
eq pre "Patrol pods Running and ready" "${notready:-none}" none
apps=$(k get applications.cd.patrol.example -l app.kubernetes.io/part-of=patrol-e2e-fixtures -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')
eq pre "fixture Applications registered" "$(grep -c . <<<"$apps")" 7
for a in $apps; do
	eq pre "$a is Synced/Healthy (run make kind-verify first)" "$(app_state "$a")" Synced/Healthy
done
unavailable=$(kt get deploy -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{" "}{.status.availableReplicas}{"/"}{.spec.replicas}{"\n"}{end}' |
	awk '$1 ~ /^e2e-/ {n++; split($2, r, "/"); if (r[1] != r[2]) print $1} END {if (n != 7) print n + 0 " e2e Deployments"}')
eq pre "the 7 e2e Deployments on $target are available" "${unavailable:-none}" none
secret_value patrol-initial-admin-secret password >"$work/pw"
eq pre "admin login with the password in patrol-initial-admin-secret" "$(login "$work/pw")" 200
cli_image=$(k get deployment patrol-server -o jsonpath='{.spec.template.spec.containers[0].image}')
if [[ -n $cli_image ]] && docker image inspect "$cli_image" >/dev/null 2>&1; then
	ok pre "the installed image $cli_image is in the local Docker (its patrol CLI runs the drill)"
else
	bad pre "the installed image '$cli_image' is not in the local Docker"
fi
write_cli_kubeconfig || bad pre "cannot write the internal kubeconfig of $control"
target_state >"$work/target-before"
eq pre "e2e-* Namespaces and Deployments recorded on $target ($(grep -c . "$work/target-before") objects)" \
	"$(awk '$1 == "Namespace" || $1 == "Deployment" {print $1}' "$work/target-before" | sort | uniq -c | awk '{printf "%s%s=%s", sep, $2, $1; sep=","}')" \
	"Deployment=7,Namespace=7"
gate "the drill (state-changing)"

echo "== backup"
k patch applications.cd.patrol.example e2e-guestbook --type=merge \
	-p '{"metadata":{"finalizers":["resources-finalizer.cd.patrol.example"]}}' >/dev/null
eq backup "e2e-guestbook carries resources-finalizer.cd.patrol.example" \
	"$(k get applications.cd.patrol.example e2e-guestbook -o jsonpath='{.metadata.finalizers}')" '["resources-finalizer.cd.patrol.example"]'
dirty="e2e-guestbook in $control/$ns carries the drill's finalizer; remove it with: kubectl -n $ns patch applications.cd.patrol.example e2e-guestbook --type=merge -p '{\"metadata\":{\"finalizers\":null}}'"
if patrol_cli admin export -n "$ns" </dev/null >"$work/backup.yaml" 2>"$work/export.err"; then
	ok backup "patrol admin export -n $ns"
else
	bad backup "patrol admin export -n $ns failed: $(grep -v '^WARNING' "$work/export.err" | tail -n 3)"
fi
eq backup "no warning from the export" "$(grep -c 'level=WARN' "$work/export.err")" 0
grep '^kind: ' "$work/backup.yaml" | sort | uniq -c | sed 's/^ */        /'
backup_names "$work/backup.yaml" >"$work/names-before" 2>"$work/out" || bad backup "the backup names objects that do not exist: $(tail -n 1 "$work/out")"
for want in application.cd.patrol.example/e2e-guestbook cluster.cd.patrol.example/patrol-target \
	secret/patrol-secret secret/cluster-patrol-target secret/e2e-git-http-creds secret/e2e-git-ssh-creds \
	secret/e2e-chart-repo-creds configmap/patrol-cm configmap/patrol-ssh-known-hosts-cm; do
	eq backup "the backup holds $want" "$(grep -cxF "$want" "$work/names-before")" 1
done
for not in secret/patrol-initial-admin-secret secret/patrol-internal-token; do
	eq backup "the backup leaves out $not" "$(grep -cxF "$not" "$work/names-before")" 0
done
helmc get values patrol -o yaml >"$work/values.yaml" 2>"$work/out" || bad backup "helm get values: $(tail -n 1 "$work/out")"
chart_of() { helmc list --filter '^patrol$' -o json | sed -n 's/.*"chart":"\([^"]*\)".*/\1/p'; }
chart=$(chart_of)
if [[ -n $chart ]]; then ok backup "installed chart: $chart"; else bad backup "cannot read the installed chart version"; fi
gate "the loss of the installation"

if [[ $new_cluster -eq 0 ]]; then
	echo "== loss: namespace $ns"
	recover="make kind-install (then make kind-verify)"
	for w in $(k get deployments,statefulsets -o name |
		grep -E '^(deployment.apps/patrol-applicationset-controller|statefulset.apps/patrol-controller)$'); do
		k scale "$w" --replicas=0 >/dev/null || bad loss "scale $w to 0"
	done
	no_controller_pods() {
		local p
		p=$(controller_pods) && [[ -z $p ]]
	}
	until_true "$(patrol::secs 90)" no_controller_pods
	eq loss "the application and ApplicationSet controllers are stopped" "$(listing controller_pods)" ""
	strip_finalizers || bad loss "removing the finalizers failed"
	eq loss "no Patrol CD object in $ns has finalizers" "$(listing finalizers_left)" ""
	eq loss "no Operation is in progress" "$(listing active_operations)" ""
	[[ $fail -eq 0 ]] || dirty="the controllers of $control/$ns are scaled to 0 and the finalizers removed; run: $recover"
	gate "the namespace deletion"
	dirty="the namespace $ns was deleted or is being deleted; if the run stopped here, run: $recover"
	patrol::kubectl "$control" delete namespace "$ns" --wait=false >/dev/null
	if patrol::kubectl "$control" wait --for=delete "namespace/$ns" --timeout="$(patrol::secs 180)s" >/dev/null 2>&1; then
		ok loss "namespace $ns deleted"
	else
		bad loss "namespace $ns still exists after $(patrol::secs 180) s"
		stop "the namespace was not deleted; run: $recover"
	fi
	eq loss "the Patrol CD CRDs remain" "$(patrol_crds | grep -c .)" 8
	eq loss "the e2e-* objects on $target are unchanged" "$(target_state | diff "$work/target-before" - >/dev/null && echo same || echo changed)" same
else
	echo "== loss: cluster $control"
	recover="make kind-up kind-fixtures kind-install (then make kind-verify)"
	# The fixture credentials, key by key; deploy.sh creates only the Secrets that are missing.
	ke() { patrol::kubectl "$control" -n patrol-e2e "$@"; }
	fixture_secrets="git-server-http-auth chart-repo-auth git-server-ssh-client git-server-ssh-authorized-keys"
	for s in $fixture_secrets; do
		mkdir -p "$work/e2e/$s"
		ke get secret "$s" -o jsonpath='{.type}' >"$work/e2e/$s.type" || bad loss "read Secret patrol-e2e/$s"
		# shellcheck disable=SC2016 # a Go template, not a shell expansion
		for key in $(ke get secret "$s" -o go-template='{{range $k, $v := .data}}{{$k}}{{"\n"}}{{end}}'); do
			ke get secret "$s" -o go-template="{{index .data \"$key\" | base64decode}}" >"$work/e2e/$s/$key" || bad loss "read $s/$key"
		done
	done
	gate "the deletion of $control"
	dirty="the kind cluster $control was deleted or is being recreated; if the run stopped here, run: $recover"
	kind delete cluster --name "$control" --kubeconfig "$(patrol::kubeconfig "$control")" >/dev/null 2>&1 ||
		stop "kind delete cluster $control failed; run: $recover"
	ok loss "kind cluster $control deleted"
	"$here/up.sh" >"$work/out" 2>&1 || {
		tail -n 5 "$work/out" >&2
		stop "hack/kind/up.sh failed; run: $recover"
	}
	ok loss "kind cluster $control created again"
	patrol::kubectl "$control" create namespace patrol-e2e >/dev/null || stop "create namespace patrol-e2e; run: $recover"
	for s in $fixture_secrets; do
		args=()
		for f in "$work/e2e/$s"/*; do args+=(--from-file="${f##*/}=$f"); done
		ke create secret generic "$s" --type="$(cat "$work/e2e/$s.type")" "${args[@]}" >/dev/null ||
			stop "create Secret patrol-e2e/$s; run: $recover"
	done
	ok loss "fixture credentials carried over to the new cluster"
	"$PATROL_ROOT/test/e2e/fixtures/deploy.sh" >"$work/out" 2>&1 || {
		tail -n 5 "$work/out" >&2
		stop "test/e2e/fixtures/deploy.sh failed; run: $recover"
	}
	ok loss "fixtures deployed again"
	"$here/load-image.sh" --cluster "$control" "$cli_image" >"$work/out" 2>&1 || stop "loading $cli_image failed; run: $recover"
	write_cli_kubeconfig || stop "cannot write the internal kubeconfig of the new $control; run: $recover"
	eq loss "the new cluster has no Patrol CD CRDs" "$(listing patrol_crds)" ""
	eq loss "the e2e-* objects on $target are unchanged" "$(target_state | diff "$work/target-before" - >/dev/null && echo same || echo changed)" same
fi

echo "== reinstall ($chart, saved values)"
if helmc install patrol "$PATROL_ROOT/deploy/helm/patrol-cd" --create-namespace --values "$work/values.yaml" \
	--wait --timeout "$(patrol::secs 360)s" >"$work/out" 2>&1; then
	ok install "helm install patrol with the saved values"
else
	bad install "helm install failed: $(tail -n 3 "$work/out")"
	stop "the release is not installed; run: $recover"
fi
eq install "the same chart version is installed" "$(chart_of)" "$chart"
until_true "$(patrol::secs 90)" healthz_ok
default_project() { k get appprojects.cd.patrol.example default >/dev/null; }
until_true "$(patrol::secs 90)" default_project
eq install "the new installation created the default AppProject" "$(k get appprojects.cd.patrol.example default -o name 2>&1)" appproject.cd.patrol.example/default
if secret_value patrol-initial-admin-secret password >"$work/pw-new" && [[ -s $work/pw-new ]]; then
	eq install "the new installation generated another admin password" "$(cmp -s "$work/pw" "$work/pw-new" && echo same || echo different)" different
else
	bad install "cannot read the admin password of the new installation"
fi
[[ $fail -eq 0 ]] || stop "the new installation is not ready; run: $recover"

echo "== import"
if patrol_cli admin import -n "$ns" - <"$work/backup.yaml" >"$work/import.out" 2>"$work/import.err"; then
	ok import "patrol admin import -n $ns"
else
	bad import "patrol admin import -n $ns failed: $(tail -n 3 "$work/import.err")"
fi
sed 's/^/        /' "$work/import.out"
eq import "the import reports success in namespace $ns" "$(grep -c "^Import process completed successfully in namespace $ns " "$work/import.out")" 1
eq import "no warning from the import" "$(grep -c 'level=WARN' "$work/import.err")" 0
grep 'level=WARN' "$work/import.err" | head -n 5 | sed 's/^/        /'
backup_names "$work/backup.yaml" >"$work/names-after" 2>"$work/out" || bad import "objects of the backup are missing: $(tail -n 1 "$work/out")"
eq import "every object of the backup exists again" "$(diff "$work/names-before" "$work/names-after" >/dev/null && echo yes || echo no)" yes
dirty="the restore did not complete in $control/$ns; run: $recover"

echo "== restored"
admin_login() { [[ "$(login "$work/pw" "$work/session.json")" == 200 ]]; }
until_true "$(patrol::secs 30)" admin_login
eq restored "admin login with the password from before the drill" "$(login "$work/pw" "$work/session.json")" 200
eq restored "the password the new installation generated is refused" "$(login "$work/pw-new")" 401
auth_header "$work/session.json" "$work/auth.hdr"
# Export leaves out status, so every Synced/Healthy below comes from a comparison by the new
# controller. The comparisons run concurrently: one deadline for all of them. It covers the
# kubelet's refresh of the restored known hosts (about a minute) and the controller's 180 s grace
# before it reports a repository error.
deadline=$((SECONDS + $(patrol::secs 360)))
for a in $apps; do
	if until_true "$(time_left "$deadline")" app_synced_healthy "$a"; then
		ok restored "$a is Synced/Healthy"
	else
		bad restored "$a is $(app_state "$a"): $(k get applications.cd.patrol.example "$a" -o jsonpath='{range .status.conditions[*]}{.type}: {.message}; {end}' | head -c 300)"
	fi
done
eq restored "no Operation was created (nothing was synced)" "$(listing k get operations.cd.patrol.example -o name)" ""
eq restored "e2e-guestbook has its finalizer again" \
	"$(k get applications.cd.patrol.example e2e-guestbook -o jsonpath='{.metadata.finalizers}')" '["resources-finalizer.cd.patrol.example"]'
private=http://chart-repo.patrol-e2e.svc.cluster.local/private
code=$(curl -sk -m "$(patrol::secs 30)" -H @"$work/auth.hdr" "$api/api/v1/repositories/http%3A%2F%2Fchart-repo.patrol-e2e.svc.cluster.local%2Fprivate" \
	-o "$work/repo.json" -w '%{http_code}')
eq restored "GET the private Helm repository through the API" "$code" 200
eq restored "its restored credential passes the connection check" \
	"$(sed -n 's/.*"connectionState":{"status":"\([A-Za-z]*\)".*/\1/p' "$work/repo.json")" Successful
eq restored "the private Helm repository refuses anonymous requests" \
	"$(patrol::kubectl "$control" -n patrol-e2e exec toolbox -- curl -s -m 8 -o /dev/null -w '%{http_code}' "$private/index.yaml" 2>&1 | tail -n 1)" 401
target_state >"$work/target-after"
if diff "$work/target-before" "$work/target-after" >"$work/target.diff"; then
	ok restored "UIDs and generations of the e2e-* objects on $target are unchanged ($(grep -c . "$work/target-after") objects)"
else
	bad restored "objects in the e2e-* namespaces on $target changed:"
	sed 's/^/        /' "$work/target.diff" | head -n 20
fi

echo "== cleanup"
if k patch applications.cd.patrol.example e2e-guestbook --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null; then
	ok cleanup "the drill's finalizer is removed from e2e-guestbook"
else bad cleanup "remove the drill's finalizer from e2e-guestbook"; fi
# The kind scripts read the admin password from patrol-initial-admin-secret.
# Some base64 builds end lines with \r\n.
printf '{"data":{"password":"%s"}}' "$(base64 <"$work/pw" | tr -d '\r\n')" >"$work/pw-patch.json"
if k patch secret patrol-initial-admin-secret --type=merge --patch-file "$work/pw-patch.json" >/dev/null; then
	ok cleanup "patrol-initial-admin-secret holds the restored admin password"
else bad cleanup "update patrol-initial-admin-secret"; fi
[[ $fail -eq 0 ]] && dirty=""

echo
echo "restore-drill: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
