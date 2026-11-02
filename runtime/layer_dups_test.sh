#!/usr/bin/env bash
# layer_dups (layer_dups.sh), fed with fixture listings: the duplicates it
# finds, an empty layer that passes, and a failing grep that fails it.
set -euo pipefail

# layer_dups.sh is linted on its own, through its sh_library.
# shellcheck source=/dev/null
source "$(cd "$(dirname "$0")" && pwd)/layer_dups.sh"

fail=0

# expect <case> <listing> <want>: layer_dups succeeds on the listing and
# prints exactly want.
expect() {
	local name="$1" listing="$2" want="$3" got
	if ! got="$(printf '%s' "${listing}" | layer_dups)"; then
		echo "FAIL ${name}: layer_dups failed" >&2
		fail=1
		return
	fi
	if [[ ${got} != "${want}" ]]; then
		printf 'FAIL %s: got %q, want %q\n' "${name}" "${got}" "${want}" >&2
		fail=1
		return
	fi
	echo "ok   ${name}"
}

expect "a directory per tarball" $'./etc/\n./etc/group\n./etc/\n./etc/passwd\n' "etc"
expect "./etc/ and etc are one path" $'./etc/\netc\n' "etc"
expect "several, sorted" $'./var/\n./etc/\n./var/\n./etc/\n' $'etc\nvar'
expect "no path twice" $'./\n./etc/\n./etc/group\n' ""
expect "the root is not a path" $'./\n./\n.\n' ""
expect "an empty layer" "" ""
expect "an empty layer, one blank line" $'\n' ""

# A grep that fails (2: an error, not "no line selected") fails layer_dups,
# and so the check, instead of passing as an empty layer would.
if (
	# Called by layer_dups, in place of the grep binary.
	# shellcheck disable=SC2329
	grep() {
		cat >/dev/null
		return 2
	}
	printf './etc/\n./etc/\n' | layer_dups >/dev/null
); then
	echo "FAIL a grep error: layer_dups succeeded" >&2
	fail=1
else
	echo "ok   a grep error"
fi

exit "${fail}"
