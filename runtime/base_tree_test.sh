#!/usr/bin/env bash
# No file in the base's tarballs is named like Claude Code (claude*, or a
# path with anthropic in it), at any path: the Debian packages
# (@bookworm//:flat, the packages layer) and the system files (:system, the
# system layer), built for one architecture. The base's two other layers
# are fixed lists in runtime/BUILD.bazel (/etc/hosts, /etc/nsswitch.conf,
# /usr/local/bin/redoma-shim).
#
# Arguments: the architecture, then the runfiles paths of the tarballs.
set -euo pipefail

arch=$1
shift

files=0
for tarball in "$@"; do
	listing="$(tar -tf "${TEST_SRCDIR}/${tarball}")"
	files=$((files + $(wc -l <<<"${listing}")))
	if found="$(grep -Ei '(^|/)claude[^/]*/?$|anthropic' <<<"${listing}")"; then
		echo "FAIL (${arch}): in ${tarball}:" >&2
		echo "${found}" >&2
		exit 1
	fi
done
((files > 1000)) || {
	echo "FAIL (${arch}): only ${files} files listed" >&2
	exit 1
}
echo "${arch}: ${files} paths, none of Claude Code's"
