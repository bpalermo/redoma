#!/usr/bin/env bash
# The base image's org.opencontainers.image.version label, in the config of
# each architecture's manifest, is the packages' version (deb_version.txt),
# and its tag that version with `-` for `~`.
#
# Arguments: the runfiles paths of the base index's OCI layout directory, of
# deb_version.txt and of the tag file.
set -euo pipefail

layout="${TEST_SRCDIR}/$1"
want="$(cat "${TEST_SRCDIR}/$2")"
tag="$(cat "${TEST_SRCDIR}/$3")"

if [[ ${tag} != "${want//\~/-}" ]]; then
	echo "FAIL: the tag is ${tag}, not ${want//\~/-}" >&2
	exit 1
fi

# The configs are the blobs with a label in them; both architectures', and
# nothing else, carries one.
configs=0
for blob in "${layout}"/blobs/sha256/*; do
	grep -q '"org.opencontainers.image.version"' "${blob}" 2>/dev/null || continue
	configs=$((configs + 1))
	got="$(grep -o '"org.opencontainers.image.version": *"[^"]*"' "${blob}" | sed 's/.*: *"\(.*\)"$/\1/')"
	if [[ ${got} != "${want}" ]]; then
		echo "FAIL: $(basename "${blob}") is labelled ${got}, not ${want}" >&2
		exit 1
	fi
done
if ((configs != 2)); then
	echo "FAIL: want the configs of 2 architectures, found ${configs}" >&2
	exit 1
fi
echo "both configs labelled ${want}, tagged ${tag}"
