#!/usr/bin/env bash
# Decides which tiers of .github/workflows/ci.yml run, from the event and the changed files.
# Writes docs_only, full and kind (true|false) to $GITHUB_OUTPUT (stdout when unset) and a short
# explanation to the step summary.
#
#   docs_only  every changed file is documentation (docs/, *.md at the repository root, .gitignore;
#              not LICENSE or NOTICE, which the image build copies, nor CHANGELOG.md, whose push is
#              the release commit): only the static job runs, for pushes and pull requests alike. A
#              README in a subdirectory counts as a change to that directory (some are test fixtures).
#   full       the expensive Go tier: unit tests under -race, the integration suite and the deploy
#              suite. True for pushes to main, manual runs (input "full", default true), pull requests
#              with the label ci:full, pull requests that touch the files below those suites depend
#              on, and every pull request when the repository variable CI_HEAVY_ON_PR is "true".
#              Otherwise the unit job runs without -race and the other two do not run.
#   kind       the image is packaged and the kind job runs. True whenever full is, for pull requests
#              with the label ci:kind, and for pull requests that touch the image, the chart, the kind
#              scripts or the CI files the kind job runs (kind_re below; workflow, setup action,
#              gate and Dependabot changes do not qualify; ci.yml and the shared setup action select
#              the full tier, and with it the kind job).
#
# Environment (set by the workflow, never interpolated into this script): EVENT_NAME, BASE_REF_NAME,
# BEFORE_SHA, PR_LABELS (JSON array of label names), DISPATCH_FULL ("true"/"false"/""), HEAVY_ON_PR
# (the CI_HEAVY_ON_PR variable). Runs in a git checkout; for pull_request events HEAD is the merge
# commit, whose first parent is the tip of the base branch.
set -euo pipefail

event=${EVENT_NAME:-}
labels=${PR_LABELS:-[]}
out=${GITHUB_OUTPUT:-/dev/stdout}
summary=${GITHUB_STEP_SUMMARY:-/dev/null}

# Files whose change makes the full Go tier / the kind job worth their minutes for a pull request.
full_re='^(\.github/workflows/ci\.yml$|\.github/actions/|hack/ci/(must-run|allowed-skips)\.txt$|hack/ci/tools\.lock$|hack/ci/testreport/|test/(deploy|integration)/|deploy/|go\.(mod|sum)$|tools/|Makefile$|hack/[^/]+\.mk$)'
kind_re='^(hack/(kind|images)/|hack/ci/(install-tools\.sh|tools\.lock|preflight\.sh|free-disk\.sh|sample-resources\.sh|kind-diagnostics\.sh|classify-changes\.sh)$|test/e2e/|deploy/|Dockerfile$|\.dockerignore$|Makefile$|hack/[^/]+\.mk$)'
docs_re='^(docs/|[^/]+\.md$|\.gitignore$)'

changed=$(mktemp)
trap 'rm -f "$changed"' EXIT
all_changed=0
case $event in
pull_request)
	# --no-renames: a file moved out of a code path must still show its old path
	git -c core.quotePath=false diff --no-renames --name-only HEAD^1 HEAD >"$changed"
	;;
push)
	before=${BEFORE_SHA:-}
	if [[ -n $before && $before != 0000000000000000000000000000000000000000 ]] && git cat-file -e "$before^{commit}" 2>/dev/null; then
		git -c core.quotePath=false diff --no-renames --name-only "$before" HEAD >"$changed"
	else
		all_changed=1
	fi
	;;
*)
	all_changed=1
	;;
esac

docs_only=false
touches_full=false
touches_kind=false
if [[ $all_changed -eq 0 ]]; then
	# CHANGELOG.md is not documentation: a release commit can change it alone (the first release,
	# or a version prepared again), and the release workflow publishes it only after a push run of
	# CI passed every job of the full tier.
	if [[ -s $changed ]] && ! grep -qvE "$docs_re" "$changed" && ! grep -qxF CHANGELOG.md "$changed"; then docs_only=true; fi
	if grep -qE "$full_re" "$changed"; then touches_full=true; fi
	if grep -qE "$kind_re" "$changed"; then touches_kind=true; fi
fi

full=false
kind=false
reason=none
case $event in
push)
	full=true kind=true reason="push to ${BASE_REF_NAME:-main}"
	;;
workflow_dispatch)
	if [[ ${DISPATCH_FULL:-true} != false ]]; then
		full=true kind=true reason="manual run with full=true"
	fi
	;;
pull_request)
	if [[ ${HEAVY_ON_PR:-} == true ]]; then
		full=true kind=true reason="repository variable CI_HEAVY_ON_PR=true"
	elif printf '%s' "$labels" | grep -qF '"ci:full"'; then
		full=true kind=true reason="label ci:full"
	else
		if [[ $touches_full == true ]]; then
			full=true kind=true reason="the pull request changes the chart, the deploy or integration suites, the modules, the pinned tools or the Makefiles"
		elif [[ $touches_kind == true ]]; then
			kind=true reason="the pull request changes the image, the kind scripts or the CI files"
		fi
		if [[ $kind == false ]] && printf '%s' "$labels" | grep -qF '"ci:kind"'; then
			kind=true reason="label ci:kind"
		fi
	fi
	;;
esac
# A documentation-only change never needs the expensive tiers, whatever else asked for them.
# docs_only can only be true for push and pull_request: other events do not compute a diff.
if [[ $docs_only == true ]]; then full=false kind=false reason="documentation only"; fi

{
	echo "docs_only=$docs_only"
	echo "full=$full"
	echo "kind=$kind"
} >>"$out"
{
	echo "### CI tiers"
	echo
	echo "- event: \`$event\`"
	echo "- documentation only: $docs_only"
	echo "- full Go tier (unit under -race, integration, deploy): $full"
	echo "- kind job: $kind"
	echo "- reason: ${reason}"
	if [[ $all_changed -eq 0 ]]; then
		echo "- changed files: $(wc -l <"$changed" | tr -d ' ')"
	else
		echo "- changed files: not computed for this event"
	fi
} >>"$summary"
