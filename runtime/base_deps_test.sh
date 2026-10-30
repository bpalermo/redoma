#!/usr/bin/env bash
# Nothing the base image or its push is built from is Claude Code's: no
# target of the claude_code repository, no //runtime:claude_layer. The
# argument is the runfiles path of the query's output (//runtime:base_deps),
# one label per line.
set -euo pipefail

deps="${TEST_SRCDIR}/$1"
if [[ ! -s ${deps} ]]; then
	echo "FAIL: ${deps} is empty" >&2
	exit 1
fi
# A query that missed the graph would pass the check below: the base's
# manifest and redoma-shim are in it.
for want in //runtime:base_manifest //shim:shim; do
	if ! grep -qx "${want}" "${deps}"; then
		echo "FAIL: ${want} is not in ${deps}" >&2
		exit 1
	fi
done
if grep -i 'claude' "${deps}" >&2; then
	echo "FAIL: the base depends on the targets above" >&2
	exit 1
fi
echo "$(wc -l <"${deps}") targets, none of Claude Code's"
