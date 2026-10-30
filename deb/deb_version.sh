#!/usr/bin/env bash
# The packages' Debian version, from VERSION and the build's stable status
# (bazel/workspace_status.sh):
#
#   a release, tagged v<VERSION>                   <VERSION>
#   a release candidate, tagged v<VERSION>-rc.<n>  <VERSION>~rc.<n>
#   a commit after candidate n of VERSION          <VERSION>~rc.<n>.git<commit time>.<commit, 7>
#   any other commit                               <VERSION>~git<commit time>.<commit, 7>
#   a build without stamping                       <VERSION>~dev
#
# A snapshot built from a tree with uncommitted changes gets ".dirty" on top.
# `~` sorts before anything, so every snapshot and candidate of a version is
# older than its release; a candidate's snapshots sort between it and the
# next candidate; snapshots of one base order by their commit time. A newer
# build always upgrades an older one.
#
# Arguments: VERSION, then the stable status file (absent without --stamp).
set -euo pipefail

version="$(tr -d '[:space:]' <"$1")"
status="${2:-}"

key() {
	[[ -r "${status}" ]] && sed -n "s/^$1 //p" "${status}" | head -1
}

tag="$(key STABLE_RELEASE_TAG || true)"
last_rc="$(key STABLE_LAST_RC_TAG || true)"
time="$(key STABLE_GIT_COMMIT_TIME || true)"
commit="$(key STABLE_GIT_COMMIT || true)"
dirty="$(key STABLE_GIT_DIRTY || true)"

# A candidate's n counts from 1, without a leading zero, as in
# bazel/workspace_status.sh; any other tag is no candidate.
rc='-rc\.([1-9][0-9]*)$'
if [[ ${tag} == "v${version}" ]]; then
	echo "${version}"
	exit 0
elif [[ ${tag} =~ ^v${version//./\\.}${rc} ]]; then
	echo "${version}~rc.${BASH_REMATCH[1]}"
	exit 0
fi

if [[ ! ${time} =~ ^[0-9]{14}$ || ! ${commit} =~ ^[0-9a-f]{7,}$ ]]; then
	echo "${version}~dev"
	exit 0
fi
snapshot="git${time}.${commit:0:7}"
if [[ ${last_rc} =~ ^v${version//./\\.}${rc} ]]; then
	snapshot="rc.${BASH_REMATCH[1]}.${snapshot}"
fi
if [[ ${dirty} == true ]]; then
	snapshot="${snapshot}.dirty"
fi
echo "${version}~${snapshot}"
