#!/usr/bin/env bash
# No layer of an image (the base, or the runtime image FROM it) lists a
# path twice. A tar with two entries for one path (./etc/ once per tarball
# flattened into a layer, say) is valid, but containers/storage refuses to
# apply it ("duplicates of file paths not supported"), so Podman cannot
# pull the image.
#
# Every layer blob of one architecture's OCI layout is listed, zstd or gzip
# (found by its magic bytes), and checked by layer_dups (layer_dups.sh).
#
# Arguments: the architecture, the runfiles path of the OCI layout
# directory of the image's index for it, and the number of layers it has.
set -euo pipefail

# layer_dups.sh is linted with this script, through its sh_library.
# shellcheck source=/dev/null
source "$(cd "$(dirname "$0")" && pwd)/layer_dups.sh"

arch=$1
layout="${TEST_SRCDIR}/$2"
want=$3

layers=0
failed=0
for blob in "${layout}"/blobs/sha256/*; do
	magic="$(head -c 4 "${blob}" | od -An -tx1 | tr -d ' \n')"
	case "${magic}" in
	28b52ffd) listing="$(zstd -dc <"${blob}" | tar -t)" ;;
	1f8b*) listing="$(gzip -dc <"${blob}" | tar -t)" ;;
	*) continue ;;
	esac
	layers=$((layers + 1))
	if ! dups="$(layer_dups <<<"${listing}")"; then
		echo "FAIL (${arch}): could not check layer $(basename "${blob}")" >&2
		exit 1
	fi
	if [[ -n ${dups} ]]; then
		echo "FAIL (${arch}): layer $(basename "${blob}") lists these paths more than once:" >&2
		echo "${dups}" >&2
		failed=1
	fi
done
if ((layers != want)); then
	echo "FAIL (${arch}): found ${layers} layers in ${layout}, want ${want}" >&2
	exit 1
fi
if ((failed)); then
	exit 1
fi
echo "${arch}: ${layers} layers, no path listed twice"
