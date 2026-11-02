#!/usr/bin/env bash
# What release-tag.sh makes of a tag: a release, a candidate, or a failure.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/release-tag.sh"
[ -r "${script}" ] || script="${TEST_SRCDIR}/_main/bazel/release-tag.sh"

dir="$(mktemp -d)"
trap 'rm -rf "${dir}"' EXIT
echo 0.1.0 >"${dir}/VERSION"
unset GITHUB_OUTPUT

fail=0
expect() { # <tag> <want deb> <want prerelease>
	local got want
	want="$(printf 'deb=%s\nprerelease=%s' "$2" "$3")"
	if ! got="$(bash "${script}" "${dir}/VERSION" "$1" 2>/dev/null)" || [[ ${got} != "${want}" ]]; then
		echo "FAIL: $1: '${got:-refused}', want '${want}'" >&2
		fail=1
	fi
}
refused() { # <tag> <why>
	if got="$(bash "${script}" "${dir}/VERSION" "$1" 2>/dev/null)"; then
		echo "FAIL: $1, $2: '${got}', want it refused" >&2
		fail=1
	fi
}

expect v0.1.0 0.1.0 false
expect v0.1.0-rc.1 0.1.0~rc.1 true
expect v0.1.0-rc.10 0.1.0~rc.10 true
refused v0.1.0-rc.0 "candidates count from 1"
refused v0.1.0-rc.01 "a leading zero"
refused v0.1.0-rc.x "not a number"
refused v0.1.0-rc.3x "not a number"
refused v0.1.0-rc. "no number"
refused v0.1.1 "another version"
refused v0.1.1-rc.1 "a candidate of another version"
refused v0x1x0 "dots are not wildcards"

# GITHUB_OUTPUT gets the same lines.
GITHUB_OUTPUT="${dir}/output" bash "${script}" "${dir}/VERSION" v0.1.0-rc.2 >/dev/null
[[ "$(cat "${dir}/output")" == "$(printf 'deb=0.1.0~rc.2\nprerelease=true')" ]] || {
	echo "FAIL: GITHUB_OUTPUT: '$(cat "${dir}/output")'" >&2
	fail=1
}

# The repository's own VERSION is a release's, never a candidate's: with
# 0.2.0-rc.1 in it, the tag v0.2.0-rc.1 would publish a final release whose
# packages are versioned 0.2.0-rc.1, a Debian revision. Its first candidate
# is a prerelease of <VERSION>~rc.1.
repo_version="${TEST_SRCDIR}/_main/VERSION"
v="$(tr -d '[:space:]' <"${repo_version}")"
if [[ ! ${v} =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
	echo "FAIL: VERSION is '${v}', not <major>.<minor>.<patch>" >&2
	fail=1
elif ! got="$(bash "${script}" "${repo_version}" "v${v}-rc.1")" || [[ ${got} != "$(printf 'deb=%s~rc.1\nprerelease=true' "${v}")" ]]; then
	echo "FAIL: v${v}-rc.1 against VERSION: '${got:-}'" >&2
	fail=1
fi

[ "${fail}" = 0 ]
echo "release tags as expected"
