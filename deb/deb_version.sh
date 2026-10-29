#!/usr/bin/env bash
# The packages' Debian version, from VERSION and the build's stable status
# (bazel/workspace_status.sh):
#
#   a release, tagged v<VERSION>                  <VERSION>
#   a release candidate, tagged v<VERSION>-rc.<n>  <VERSION>~rc.<n>
#   any other commit                              <VERSION>~git<commit time>.<commit, 7>
#   a build without stamping                      <VERSION>~dev
#
# `~` sorts before anything, so every snapshot and candidate of a version is
# older than its release, and snapshots order by their commit time: a newer
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
time="$(key STABLE_GIT_COMMIT_TIME || true)"
commit="$(key STABLE_GIT_COMMIT || true)"

case "${tag}" in
"v${version}")
	echo "${version}"
	;;
"v${version}-rc."*)
	echo "${version}~rc.${tag##*-rc.}"
	;;
*)
	if [[ ${time} =~ ^[0-9]{14}$ && ${commit} =~ ^[0-9a-f]{7,}$ ]]; then
		echo "${version}~git${time}.${commit:0:7}"
	else
		echo "${version}~dev"
	fi
	;;
esac
