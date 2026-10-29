#!/usr/bin/env bash
# deb_version.sh: a release, a candidate, a candidate's snapshot, a
# snapshot, a dirty tree and an unstamped build, and that the versions order
# as dpkg must upgrade them.
#
# Arguments: the runfiles path of deb_version.sh.
set -euo pipefail

script="${TEST_SRCDIR}/$1"
dir="${TEST_TMPDIR}/version"
mkdir -p "${dir}"
echo 0.2.0 >"${dir}/VERSION"

fail=0
expect() { # <want> <what> [status lines]
	local want=$1 what=$2 got
	shift 2
	printf '%s\n' "$@" >"${dir}/status"
	got="$(bash "${script}" "${dir}/VERSION" "${dir}/status")"
	if [[ ${got} != "${want}" ]]; then
		echo "FAIL: ${what}: '${got}', want '${want}'" >&2
		fail=1
	fi
}

commit=0123456789abcdef0123456789abcdef01234567
at="STABLE_GIT_COMMIT_TIME 20261029120000"
expect 0.2.0 "a release" "STABLE_RELEASE_TAG v0.2.0" "${at}" "STABLE_GIT_COMMIT ${commit}"
expect 0.2.0~rc.3 "a candidate" "STABLE_RELEASE_TAG v0.2.0-rc.3" "STABLE_LAST_RC_TAG v0.2.0-rc.3" "STABLE_GIT_COMMIT ${commit}"
expect 0.2.0~git20261029120000.0123456 "a snapshot" "STABLE_RELEASE_TAG none" "STABLE_LAST_RC_TAG none" "${at}" "STABLE_GIT_COMMIT ${commit}"
expect 0.2.0~rc.3.git20261029120000.0123456 "a snapshot after a candidate" "STABLE_RELEASE_TAG none" "STABLE_LAST_RC_TAG v0.2.0-rc.3" "${at}" "STABLE_GIT_COMMIT ${commit}"
expect 0.2.0~git20261029120000.0123456 "a candidate of another version" "STABLE_RELEASE_TAG none" "STABLE_LAST_RC_TAG v0.1.0-rc.3" "${at}" "STABLE_GIT_COMMIT ${commit}"
expect 0.2.0~git20261029120000.0123456 "a tag of another version" "STABLE_RELEASE_TAG v0.1.0" "${at}" "STABLE_GIT_COMMIT ${commit}"
expect 0.2.0~git20261029120000.0123456.dirty "a dirty tree" "STABLE_RELEASE_TAG none" "${at}" "STABLE_GIT_COMMIT ${commit}" "STABLE_GIT_DIRTY true"
expect 0.2.0~rc.3.git20261029120000.0123456.dirty "a dirty tree after a candidate" "STABLE_RELEASE_TAG none" "STABLE_LAST_RC_TAG v0.2.0-rc.3" "${at}" "STABLE_GIT_COMMIT ${commit}" "STABLE_GIT_DIRTY true"
expect 0.2.0~dev "no commit known" "STABLE_RELEASE_TAG none" "STABLE_GIT_COMMIT unknown"
got="$(bash "${script}" "${dir}/VERSION")"
[[ ${got} == 0.2.0~dev ]] || {
	echo "FAIL: no status file: '${got}'" >&2
	fail=1
}

# The order dpkg upgrades by: an older version, then for 0.2.0 an unstamped
# build, snapshots by time, candidate 1, its snapshots, candidate 2 (and 10),
# the release. dpkg is required: the order is the point of the versions.
if ! command -v dpkg >/dev/null; then
	echo "FAIL: dpkg is not installed; it checks the order (AGENTS.md, build requirements)" >&2
	exit 1
fi
prev=""
for v in \
	0.1.0 \
	0.2.0~dev \
	0.2.0~git20261029120000.fffffff \
	0.2.0~git20261029120000.fffffff.dirty \
	0.2.0~git20261029130000.0000000 \
	0.2.0~rc.1 \
	0.2.0~rc.1.git20261029120000.0000000 \
	0.2.0~rc.1.git20261029130000.0000000 \
	0.2.0~rc.2 \
	0.2.0~rc.2.git20261029140000.0000000 \
	0.2.0~rc.10 \
	0.2.0; do
	if [[ -n ${prev} ]] && ! dpkg --compare-versions "${prev}" lt "${v}"; then
		echo "FAIL: ${prev} does not sort before ${v}" >&2
		fail=1
	fi
	prev=${v}
done

[ "${fail}" = 0 ]
echo "deb versions as expected"
