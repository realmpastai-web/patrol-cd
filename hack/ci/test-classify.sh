#!/usr/bin/env bash
# Regression tests of hack/ci/classify-changes.sh: which tiers a change selects. Each case builds a
# pull request merge commit (or a push range) in a scratch repository and compares the script's
# outputs with the expected docs_only/full/kind. Needs git only; runs in well under a second per case.
#
#   hack/ci/test-classify.sh
#
# A new shared CI input (a composite action, a lock file) belongs in classify-changes.sh's full_re or
# kind_re and in a case below: a change to it must run the jobs that use it before the merge.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script=$here/classify-changes.sh
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid

repo=$work/repo
git init -q -b main "$repo"
cd "$repo"
git commit -q --allow-empty -m base
base=$(git rev-parse HEAD)

passed=0
failed=0

# touch_files FILE... : appends a line to each file (creating it and its directories).
touch_files() {
	local f
	for f in "$@"; do
		mkdir -p "$(dirname "$f")"
		echo "$RANDOM" >>"$f"
	done
}

# classify EVENT LABELS HEAVY DISPATCH_FULL FILE... : prints "docs_only=.. full=.. kind=.." for a
# change of FILEs. EVENT pull_request: a merge commit of the change into main; push: the range from
# main to the change.
classify() {
	local event=$1 labels=$2 heavy=$3 dispatch=$4 out before
	shift 4
	git checkout -q -B change "$base"
	touch_files "$@"
	git add -A
	git commit -q -m change
	case $event in
	pull_request)
		git checkout -q -B merge "$base"
		git merge -q --no-ff -m merge change
		before=
		;;
	*)
		before=$base
		;;
	esac
	out=$(EVENT_NAME=$event PR_LABELS=$labels HEAVY_ON_PR=$heavy DISPATCH_FULL=$dispatch BEFORE_SHA=$before \
		GITHUB_OUTPUT=/dev/stdout GITHUB_STEP_SUMMARY=/dev/null "$script" | tr '\n' ' ')
	printf '%s\n' "${out% }"
}

# want NAME EXPECTED EVENT LABELS HEAVY DISPATCH_FULL FILE...
want() {
	local name=$1 expected=$2 got
	shift 2
	got=$(classify "$@")
	if [[ $got == "$expected" ]]; then
		passed=$((passed + 1))
		printf 'ok   %s\n' "$name"
	else
		failed=$((failed + 1))
		printf 'FAIL %s\n     want: %s\n     got:  %s\n' "$name" "$expected" "$got"
	fi
}

DOCS="docs_only=true full=false kind=false"
CODE="docs_only=false full=false kind=false"
KIND="docs_only=false full=false kind=true"
FULL="docs_only=false full=true kind=true"
PR=pull_request

# Pull requests: documentation only
want "docs file"                    "$DOCS" $PR '[]' '' '' docs/guide.md
want "root markdown"                "$DOCS" $PR '[]' '' '' README.md
want "docs with the ci:full label"  "$DOCS" $PR '["ci:full"]' '' '' docs/guide.md
want "docs with CI_HEAVY_ON_PR"     "$DOCS" $PR '[]' true '' docs/guide.md
want "a README below the root is code" "$CODE" $PR '[]' '' '' internal/x/README.md

# Pull requests: code
want "Go file"                      "$CODE" $PR '[]' '' '' internal/x/x.go
want "LICENSE is not documentation" "$CODE" $PR '[]' '' '' LICENSE
want "NOTICE is not documentation"  "$CODE" $PR '[]' '' '' NOTICE
want "the changelog is not documentation" "$CODE" $PR '[]' '' '' CHANGELOG.md
want "Dependabot configuration"     "$CODE" $PR '[]' '' '' .github/dependabot.yml
want "the gate script"              "$CODE" $PR '[]' '' '' hack/ci/gate.sh
want "label ci:kind"                "$KIND" $PR '["ci:kind"]' '' '' internal/x/x.go
want "label ci:full"                "$FULL" $PR '["ci:full"]' '' '' internal/x/x.go
want "CI_HEAVY_ON_PR"               "$FULL" $PR '[]' true '' internal/x/x.go

# Pull requests: the kind tier
want "kind script"                  "$KIND" $PR '[]' '' '' hack/kind/up.sh
want "kind fixtures"                "$KIND" $PR '[]' '' '' test/e2e/fixtures/deploy.sh
want "kind diagnostics"             "$KIND" $PR '[]' '' '' hack/ci/kind-diagnostics.sh
want "Dockerfile"                   "$KIND" $PR '[]' '' '' Dockerfile
want "image tool pins"              "$KIND" $PR '[]' '' '' hack/images/checksums.txt

# Pull requests: the full tier, including the shared inputs of every job
want "the workflow"                 "$FULL" $PR '[]' '' '' .github/workflows/ci.yml
want "the shared setup action"      "$FULL" $PR '[]' '' '' .github/actions/setup/action.yml
want "any composite action"         "$FULL" $PR '[]' '' '' .github/actions/other/action.yml
want "the pinned tools"             "$FULL" $PR '[]' '' '' hack/ci/tools.lock
want "make includes"                "$FULL" $PR '[]' '' '' hack/ci.mk
want "the root Makefile"            "$FULL" $PR '[]' '' '' Makefile
want "the module files"             "$FULL" $PR '[]' '' '' go.mod
want "the chart"                    "$FULL" $PR '[]' '' '' deploy/helm/patrol-cd/values.yaml
want "the deploy suite"             "$FULL" $PR '[]' '' '' test/deploy/render_test.go
want "the integration suite"        "$FULL" $PR '[]' '' '' test/integration/stack/env.go
want "the test report tool"         "$FULL" $PR '[]' '' '' hack/ci/testreport/main.go
want "the must-run rules"           "$FULL" $PR '[]' '' '' hack/ci/must-run.txt
want "a change mixing docs and the setup action" "$FULL" $PR '[]' '' '' docs/guide.md .github/actions/setup/action.yml

# Pushes and manual runs
want "push to main"                 "$FULL" push '[]' '' '' internal/x/x.go
want "docs-only push"               "$DOCS" push '[]' '' '' docs/guide.md
want "a changelog-only push (a release commit)" "$FULL" push '[]' '' '' CHANGELOG.md
want "a push of the changelog and docs" "$FULL" push '[]' '' '' CHANGELOG.md docs/guide.md
want "manual run, full"             "$FULL" workflow_dispatch '[]' '' true internal/x/x.go
want "manual run, default"          "$FULL" workflow_dispatch '[]' '' '' internal/x/x.go
want "manual run, not full"         "$CODE" workflow_dispatch '[]' '' false internal/x/x.go

want "scheduled run"                "$FULL" schedule '[]' '' '' internal/x/x.go
want "scheduled run, docs only"     "$FULL" schedule '[]' '' '' docs/guide.md
printf '\ntest-classify: %d passed, %d failed\n' "$passed" "$failed"
[[ $failed -eq 0 ]]
