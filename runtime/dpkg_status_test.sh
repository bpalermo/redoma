#!/usr/bin/env bash
# The image's /var/lib/dpkg/status names every Debian package installed in
# it, and nothing else, for one architecture: what scanners (Trivy, Grype,
# SBOM tools) read to know what the image holds. Each entry has Package,
# Version, Architecture (the image's, or all) and "Status: install ok
# installed".
#
# The installed packages are the data tarballs of @bookworm//:packages, the
# whole resolved closure in MODULE.bazel.lock, built for the architecture:
# each sits in its package's repository, named
# <suite>_<package>-<arch>_<version>.
#
# Arguments: the architecture, the runfiles path of the system tar
# (//runtime:system) built for it, then those of the data tarballs.
set -euo pipefail

arch=$1
system="${TEST_SRCDIR}/$2"
shift 2

fail() {
	echo "FAIL (${arch}): $*" >&2
	exit 1
}

tmp="${TEST_TMPDIR}/${arch}"
mkdir -p "${tmp}"

for data in "$@"; do
	repo="$(basename "$(dirname "${data}")")"
	repo="${repo##*+}"
	if [[ ! ${repo} =~ ^[^_]+_(.+)-${arch}_[^_].*$ ]]; then
		fail "a data tarball outside a package repository of ${arch}: ${data}"
	fi
	echo "${BASH_REMATCH[1]}"
done | sort -u >"${tmp}/installed"

tar -xOf "${system}" ./var/lib/dpkg/status >"${tmp}/status" ||
	fail "no ./var/lib/dpkg/status in ${system}"

# One line per entry: Package, Version, Architecture, Status.
awk -v RS= -F '\n' '{
	p = v = a = s = "-"
	for (i = 1; i <= NF; i++) {
		if ($i ~ /^Package: /) p = substr($i, 10)
		else if ($i ~ /^Version: /) v = substr($i, 10)
		else if ($i ~ /^Architecture: /) a = substr($i, 15)
		else if ($i ~ /^Status: /) s = substr($i, 9)
	}
	gsub(/ /, "_", s)
	print p, v, a, s
}' "${tmp}/status" >"${tmp}/entries"

while read -r package version architecture status; do
	[[ ${package} != - ]] || fail "an entry with no Package"
	[[ ${version} != - ]] || fail "${package}: no Version"
	[[ ${architecture} == "${arch}" || ${architecture} == all ]] ||
		fail "${package}: Architecture ${architecture}"
	[[ ${status} == install_ok_installed ]] || fail "${package}: Status ${status}"
done <"${tmp}/entries"

cut -d' ' -f1 "${tmp}/entries" | sort >"${tmp}/listed"
if [[ -n "$(uniq -d "${tmp}/listed")" ]]; then
	fail "listed twice: $(uniq -d "${tmp}/listed" | tr '\n' ' ')"
fi

missing="$(comm -23 "${tmp}/installed" "${tmp}/listed" | tr '\n' ' ')"
extra="$(comm -13 "${tmp}/installed" "${tmp}/listed" | tr '\n' ' ')"
if [[ -n ${missing} ]]; then
	fail "installed but not in the status, add them to apt.install in MODULE.bazel: ${missing}"
fi
if [[ -n ${extra} ]]; then
	fail "in the status but not installed: ${extra}"
fi

installed="$(wc -l <"${tmp}/installed")"
((installed > 16)) || fail "only ${installed} packages installed: the data tarballs were not found"
echo "${arch}: ${installed} packages installed, all in the status"
