#!/usr/bin/env bash
# The targets a revision range impacts, with Tinder/bazel-diff, as the lists
# CI builds and tests from (.github/workflows/ci.yml, the `diff` job). A
# change pays only for what it can affect: a documentation-only change builds
# nothing and tests nothing.
#
# Written to OUT_DIR:
#   impacted_build.txt   impacted ∩ the buildable rules (nothing tagged manual)
#   impacted_test.txt    impacted ∩ the tests `bazel test //...` runs (nothing
#                        tagged manual)
#   impacted_commit.txt  the commit the lists are for
# Appended to GITHUB_OUTPUT when it is set, and printed:
#   has_any    impacted_build.txt is not empty: the build job runs
#   has_test   impacted_test.txt is not empty: the build job runs the tests
#
# Fallback. On any error, and without a base (BASE_SHA empty or unreachable,
# no bazel-diff), everything is impacted: impacted_build.txt is `//...`,
# impacted_test.txt every runnable test, every has_* true. CI never silently
# under-tests. An empty impacted set is a skip only when bazel-diff and every
# query succeeded; when even the full run cannot list the tests (a `bazel
# query` that fails or returns nothing) the script exits 1 and sets no
# output, so the caller's step fails: an empty list never stands in for
# "nothing to test".
#
# HEAD_SHA must be the commit checked out, and the script fails when it is
# not: the lists are read by jobs that check that commit out themselves. It
# checks out BASE_SHA and HEAD_SHA in turn to hash each, so run it from a
# copy outside the tree (the workflow copies it to $RUNNER_TEMP): a checkout
# would otherwise swap the script file out from under bash.
#
# Seed files: a change to one of these marks every target impacted. They
# change the build or CI itself and are not in the graph bazel-diff hashes.
#
# Env:
#   BASE_SHA       the base to diff against; empty: full run
#   HEAD_SHA       the head to diff; default the checkout's HEAD, and any
#                  other commit than the checkout's is refused
#   BAZEL_DIFF     a bazel-diff executable to use instead of the pinned one
#                  (@multitool//tools/bazel-diff, multitool.lock.json); the
#                  pinned one is what CI runs
#   OUT_DIR        where the lists go; default $PWD/.bazel-diff-out
#   GITHUB_OUTPUT  appended to when set
#
# Locally, from the root of a clone, never the checkout being worked in (it
# checks revisions out), exactly as CI runs it:
#   BASE_SHA=origin/main bazel/ci-impacted-targets.sh
#
# Refusals (exit 1, no output set): HEAD_SHA is not the checkout; the
# MODULE.bazel.lock in the index or the working tree differs from HEAD (it
# would be lost to the checkouts: use a throwaway clone); the full-run
# fallback cannot list the targets.
#
# The decision after bazel-diff, `classify`, is a function on files, covered
# by ci_impacted_targets_test.sh without Bazel; the test sources this file,
# which then defines its functions and returns.
set -uo pipefail

# What CI may build and test: nothing tagged `manual`. The lists reach `bazel
# build` and `bazel test` as explicit labels (--target_pattern_file), and
# Bazel honours `manual` only when expanding a wildcard, so a `manual` target
# has to leave the candidate set here or it is built and run. `attr` matches
# a regex against the tag list printed as `[a, b, c]`: the brackets pin a
# whole tag, where a bare `manual` would also take `no-manual-x`.
MANUAL='attr("tags", "[\[ ]manual[,\]]", //...)'
BUILDABLE_RULES="kind(rule, //...) except ${MANUAL}"
RUNNABLE_TESTS="tests(//...) except ${MANUAL}"

emit() { # name value
	[ -n "${GITHUB_OUTPUT:-}" ] && echo "$1=$2" >>"${GITHUB_OUTPUT}"
	echo "  output: $1=$2"
}

die() { # message: what to build could not be determined, and nothing wider is left
	echo "::error::ci-impacted-targets: $1" >&2
	exit 1
}

# query <outfile> <expression>: the sorted unique labels, or status 1.
#
# A query that fails, and one that succeeds with nothing, both return 1. Each
# list asked for here is certain to be non-empty in this repository (it has
# rules and tests), so an empty answer is a broken query (a regex that
# matches nothing still exits 0) and never "nothing to test". Left
# unchecked, it would empty a candidate list: every has_* false, every job
# skipped and `ci` green over nothing.
query() {
	if ! "${BAZEL}" query "$2" --noshow_progress >"$1.raw" 2>"$1.err"; then
		cat "$1.err" >&2
		echo ">> bazel query failed: $2" >&2
		return 1
	fi
	# No list from an earlier run into the same OUT_DIR, and no partial one: a
	# sort or a write that fails is a failed query too.
	rm -f "$1"
	if ! sort -u "$1.raw" >"$1.sorted"; then
		echo ">> could not sort the answer of: $2" >&2
		return 1
	fi
	grep -v '^[[:space:]]*$' "$1.sorted" >"$1"
	if [ "$?" -gt 1 ] || [ ! -s "$1" ]; then
		cat "$1.err" >&2
		echo ">> bazel query returned nothing: $2" >&2
		return 1
	fi
}

# candidate_lists: all_rules.txt and all_tests.txt in OUT_DIR, or status 1.
candidate_lists() {
	query "${OUT_DIR}/all_rules.txt" "${BUILDABLE_RULES}" || return 1
	query "${OUT_DIR}/all_tests.txt" "${RUNNABLE_TESTS}" || return 1
}

# keep <labels> <candidates>: the candidates that are in <labels>, on
# stdout. grep exits 1 when there are none, which is an answer; above 1 it
# could not read a file, which is not. An empty <labels> keeps nothing.
keep() {
	grep -Fxf "$1" "$2"
	[ "$?" -le 1 ]
}

nonempty() { [ -s "$1" ] && echo true || echo false; }

# classify <impacted> <rules> <tests> <out dir>
#
# The decision: from the sorted impacted labels and the two candidate lists,
# writes impacted_build.txt and impacted_test.txt into <out dir> and sets
# has_any and has_test. Status 1 when a list could not be written. Source and
# generated-file labels in <impacted> are in no candidate list, so they are
# dropped here; so is a `manual` rule.
classify() {
	local impacted="$1" rules="$2" tests="$3" out="$4"
	keep "${impacted}" "${rules}" >"${out}/impacted_build.txt" || return 1
	keep "${impacted}" "${tests}" >"${out}/impacted_test.txt" || return 1
	has_any="$(nonempty "${out}/impacted_build.txt")"
	has_test="$(nonempty "${out}/impacted_test.txt")"
}

# Sourced for its functions: define them and stop here.
if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then
	return 0
fi

BAZEL="$(command -v bazelisk || command -v bazel)" || die "neither bazelisk nor bazel is on PATH"
OUT_DIR="${OUT_DIR:-${PWD}/.bazel-diff-out}"
mkdir -p "${OUT_DIR}" || die "could not create ${OUT_DIR}"

# The commit under test is the one checked out. Before anything else, the
# full-run fallbacks included: they list the tests of the checkout.
start_commit="$(git rev-parse HEAD 2>/dev/null)" || die "git rev-parse HEAD failed: not a git checkout"
HEAD_SHA="${HEAD_SHA:-${start_commit}}"
head_commit="$(git rev-parse --verify -q "${HEAD_SHA}^{commit}")" || die "HEAD_SHA ${HEAD_SHA} is not a commit of this repository"
[ "${head_commit}" = "${start_commit}" ] ||
	die "HEAD_SHA is ${head_commit} and the checkout is at ${start_commit}: the lists would be for another tree than the one the caller builds"
# No commit file from an earlier run into the same OUT_DIR: a run that fails
# leaves none, so its lists cannot pass the check of the job that reads them.
rm -f "${OUT_DIR}/impacted_commit.txt" || die "could not remove a stale impacted_commit.txt"
# stamp: the lists in OUT_DIR are for start_commit. Called once they are written.
stamp() {
	echo "${start_commit}" >"${OUT_DIR}/impacted_commit.txt" || die "could not write impacted_commit.txt"
}

# full_run <reason>: everything impacted (build //..., run every test) and
# exit 0. The answer to every "the impacted set could not be computed" below.
# It needs the test list itself; when it cannot get it there is nothing wider
# to fall back to, so it fails the step.
full_run() {
	echo ">> bazel-diff fallback (full run): $1" >&2
	candidate_lists || die "the full run could not list the targets (see the query above); refusing to report an empty test set"
	echo '//...' >"${OUT_DIR}/impacted_build.txt" || die "could not write impacted_build.txt"
	cp "${OUT_DIR}/all_tests.txt" "${OUT_DIR}/impacted_test.txt" || die "could not write impacted_test.txt"
	stamp
	emit has_any true
	emit has_test true
	echo ">> impacted: everything (full run); test=$(wc -l <"${OUT_DIR}/impacted_test.txt")"
	exit 0
}

# pinned_bazel_diff builds the pinned bazel-diff and copies it out of the
# output tree into OUT_DIR, printing its path. Before any revision is
# checked out: a checkout of the base could change or remove the pin, and
# the next build would swap the binary under the run.
pinned_bazel_diff() {
	local file
	"${BAZEL}" build --noshow_progress @multitool//tools/bazel-diff >&2 || return 1
	file="$("${BAZEL}" cquery --noshow_progress --output=files @multitool//tools/bazel-diff 2>/dev/null)" || return 1
	[ -n "${file}" ] || return 1
	cp -L "$("${BAZEL}" info execution_root 2>/dev/null)/${file}" "${OUT_DIR}/bazel-diff" || return 1
	chmod +x "${OUT_DIR}/bazel-diff" || return 1
	echo "${OUT_DIR}/bazel-diff"
}

# discard_lock_rewrites (below) may only undo what Bazel writes during the
# run: a lockfile edit already in the index or the working tree is
# someone's, and is kept by refusing to run, before the first Bazel command
# can touch it. git status, not git diff HEAD: an edit staged and then
# reverted in the working tree (MM) matches HEAD in the working tree, and
# the checkout would still drop the staged one.
lock_status="$(git status --porcelain -- MODULE.bazel.lock)" || die "git status of MODULE.bazel.lock failed"
[ -z "${lock_status}" ] ||
	die "MODULE.bazel.lock has uncommitted changes (${lock_status}); run this from a throwaway clone (it checks revisions out)"

if [ -z "${BAZEL_DIFF:-}" ]; then
	BAZEL_DIFF="$(pinned_bazel_diff)" || full_run "could not build the pinned bazel-diff"
fi
[ -x "${BAZEL_DIFF}" ] || full_run "no bazel-diff executable at ${BAZEL_DIFF}"
[ -n "${BASE_SHA:-}" ] || full_run "no BASE_SHA"
git cat-file -e "${BASE_SHA}^{commit}" 2>/dev/null || full_run "BASE_SHA ${BASE_SHA} unreachable"

# Seed files: hashed into every target, so a change to one of them is a full
# run. They are the inputs CI depends on that no target depends on. The lint
# aspects and their configs are here because .bazelrc attaches them to every
# build: a .bzl loaded only from .bazelrc, or a config a .bzl names by label,
# is in no target's graph, so bazel-diff would see nothing impacted and the
# threshold or rule it changed would never be checked.
SEED="${OUT_DIR}/seed.txt"
printf '%s\n' \
	.bazelrc \
	.bazelversion \
	.github/workflows/ci.yml \
	.shellcheckrc \
	MODULE.bazel \
	MODULE.bazel.lock \
	bazel/ci-impacted-targets.sh \
	bazel/lint/gocognit.bzl \
	bazel/lint/linters.bzl \
	multitool.lock.json >"${SEED}" || die "could not write ${SEED}"

# discard_lock_rewrites undoes what Bazel itself wrote to MODULE.bazel.lock in
# this throwaway checkout, which had no edit of it when the run started
# (checked above): bazel-diff's queries evaluate module extensions no
# build of this repository does and record them, and git then refuses to
# check out a revision whose lockfile differs.
discard_lock_rewrites() {
	git checkout -q -- MODULE.bazel.lock 2>/dev/null || true
}

generate() { # ref outfile
	discard_lock_rewrites
	git checkout -q --detach "$1" || return 1
	# bazel-diff errors on a seed path that does not exist at the checked-out
	# revision, so pass only the seed files present there. A seed file present
	# at one revision and not the other still flips every target's hash (a
	# full run), which is what a new or removed CI file deserves.
	local rev_seed="${SEED}.rev" p
	: >"${rev_seed}"
	while IFS= read -r p; do [ -e "${p}" ] && printf '%s\n' "${p}" >>"${rev_seed}"; done <"${SEED}"
	"${BAZEL_DIFF}" generate-hashes -w "${PWD}" -b "${BAZEL}" -s "${rev_seed}" "$2"
}

# Where the checkout is now: at HEAD_SHA (checked above), so the full run and
# the bazel-diff path list the same commit's targets.
orig_ref="$(git rev-parse --abbrev-ref HEAD)"
[ "${orig_ref}" = "HEAD" ] && orig_ref="${start_commit}"
# restore: back to where the run started, or fail. From a checkout left on
# the base revision the full run would list the BASE's tests: a test the
# change adds would be in no list, and nothing would say so.
restore() {
	discard_lock_rewrites
	git checkout -q "${orig_ref}" 2>/dev/null || git checkout -q --detach "${start_commit}" 2>/dev/null
	[ "$(git rev-parse HEAD 2>/dev/null)" = "${start_commit}" ] ||
		die "could not check out ${orig_ref} again after bazel-diff; the checkout is not at the commit under test"
}

# The sort is part of the condition: a get-impacted-targets that exits 0
# without writing its list is a failure too, not an empty impact set. And a
# list an earlier run left in OUT_DIR is not this run's answer.
rm -f "${OUT_DIR}/impacted.txt" "${OUT_DIR}/impacted.sorted"
if ! { generate "${BASE_SHA}" "${OUT_DIR}/base.json" &&
	generate "${HEAD_SHA}" "${OUT_DIR}/head.json" &&
	"${BAZEL_DIFF}" get-impacted-targets -w "${PWD}" -b "${BAZEL}" \
		--startingHashes "${OUT_DIR}/base.json" --finalHashes "${OUT_DIR}/head.json" \
		-o "${OUT_DIR}/impacted.txt" &&
	sort -u "${OUT_DIR}/impacted.txt" >"${OUT_DIR}/impacted.sorted"; }; then
	restore
	full_run "bazel-diff command failed"
fi
restore
# The candidate lists, at the commit under test. A query that fails takes the
# same fallback as a failed bazel-diff; the full run asks for them again, so
# a transient failure costs a full run and a persistent one fails the step.
candidate_lists || full_run "a classification query failed or returned nothing"

classify "${OUT_DIR}/impacted.sorted" "${OUT_DIR}/all_rules.txt" "${OUT_DIR}/all_tests.txt" "${OUT_DIR}" ||
	die "could not write the impacted lists"

stamp
emit has_any "${has_any}"
emit has_test "${has_test}"

echo ">> impacted: $(wc -l <"${OUT_DIR}/impacted.sorted") target(s); build=$(wc -l <"${OUT_DIR}/impacted_build.txt") test=$(wc -l <"${OUT_DIR}/impacted_test.txt")"
