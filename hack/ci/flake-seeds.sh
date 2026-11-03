#!/usr/bin/env bash
# Collects what is needed to reproduce a failure of the flake hunt (.github/workflows/scheduled.yml,
# `make ci-flake-*`): the shuffle seed of every package from the `go test -json` streams in DIR
# (`-test.shuffle N`), the rapid reproduction lines (`-rapid.failfile=... (or -rapid.seed=N)`), and the
# vitest seed. New rapid fail files (untracked files under a testdata/rapid directory) are copied to
# DIR/rapid with their paths, so that the artifact can be unpacked over a checkout. Writes DIR/seeds.md
# and appends it to the step summary when there is one. jq is required (runners have it).
#
# Usage: hack/ci/flake-seeds.sh DIR
set -euo pipefail

dir=${1:?usage: flake-seeds.sh DIR}
cd "$(git rev-parse --show-toplevel)"
mkdir -p "$dir"
out=$dir/seeds.md
command -v jq >/dev/null 2>&1 || {
	echo "flake-seeds: jq is not installed" >&2
	exit 1
}

{
	echo "### Flake hunt seeds"
	echo
	found=0
	for stream in "$dir"/unit.json "$dir"/integration.json; do
		[[ -f $stream ]] || continue
		found=1
		echo "#### $(basename "$stream")"
		echo
		echo '```'
		# Lines that are not JSON (a crash before the stream started) are skipped by fromjson?.
		jq -R -r 'fromjson? | select(.Action == "output" and .Output != null)
			| select(.Output | test("^-test\\.shuffle [0-9]+") or test("-rapid\\.(failfile|seed)="))
			| "\(.Package)\(if .Test then " " + .Test else "" end): \(.Output | rtrimstr("\n") | ltrimstr(" "))"' "$stream" |
			sort -u
		echo '```'
		echo
	done
	if [[ -f $dir/ui-shuffle-seed.txt ]]; then
		found=1
		echo "#### web UI"
		echo
		echo "\`$(cat "$dir/ui-shuffle-seed.txt")\` (reproduce with \`pnpm -C ui exec vitest run --sequence.shuffle --sequence.seed=N\`)"
		echo
	fi
	[[ $found -eq 1 ]] || echo "No test streams in $dir."
	echo
	echo "Reproduce a package's order with \`go test -race -shuffle=N -run ... ./pkg\`."
} >"$out"

# New rapid fail files: untracked, not ignored, below a testdata/rapid directory.
copied=0
while IFS= read -r -d '' f; do
	case $f in
	*/testdata/rapid/*)
		mkdir -p "$dir/rapid/$(dirname "$f")"
		cp "$f" "$dir/rapid/$f"
		copied=$((copied + 1))
		;;
	esac
done < <(git ls-files -z --others --exclude-standard)
echo "New rapid fail files copied to $dir/rapid: $copied" >>"$out"

if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then cat "$out" >>"$GITHUB_STEP_SUMMARY"; fi
# The lines come from test output: stop workflow commands while they are printed (as hack/ci.mk does).
tok=
if [[ ${GITHUB_ACTIONS:-} == true ]]; then tok=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n'); fi
if [[ -n $tok ]]; then echo "::stop-commands::$tok"; fi
cat "$out"
if [[ -n $tok ]]; then echo "::$tok::"; fi
