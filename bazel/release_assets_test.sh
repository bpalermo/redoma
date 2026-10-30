#!/usr/bin/env bash
# What release-assets.sh uploads for a version: the packages under names
# GitHub keeps, and a SHA256SUMS of exactly those names; or nothing.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/release-assets.sh"
[ -r "${script}" ] || script="${TEST_SRCDIR}/_main/bazel/release-assets.sh"

dir="$(mktemp -d)"
trap 'rm -rf "${dir}"' EXIT
mkdir "${dir}/bin"
echo amd64 >"${dir}/bin/redoma_amd64_deb.deb"
echo arm64 >"${dir}/bin/redoma_arm64_deb.deb"

fail=0
n=0
expect() { # <deb version> <want asset version>
	local out="${dir}/out$((n += 1))" want got arch
	if ! bash "${script}" "$1" "${dir}/bin" "${out}" >/dev/null 2>&1; then
		echo "FAIL: $1: refused, want $2" >&2
		fail=1
		return
	fi
	want="$(printf '%s\n' SHA256SUMS "redoma_$2_amd64.deb" "redoma_$2_arm64.deb")"
	got="$(ls "${out}")"
	if [[ ${got} != "${want}" ]]; then
		echo "FAIL: $1: files '${got}', want '${want}'" >&2
		fail=1
		return
	fi
	# SHA256SUMS names the uploaded files, and only them.
	got="$(awk '{print $2}' "${out}/SHA256SUMS")"
	want="$(printf '%s\n' "redoma_$2_amd64.deb" "redoma_$2_arm64.deb")"
	if [[ ${got} != "${want}" ]]; then
		echo "FAIL: $1: SHA256SUMS lists '${got}', want '${want}'" >&2
		fail=1
	fi
	if ! (cd "${out}" && sha256sum --check --strict --quiet SHA256SUMS); then
		echo "FAIL: $1: SHA256SUMS does not check" >&2
		fail=1
	fi
	for arch in amd64 arm64; do
		if ! cmp -s "${dir}/bin/redoma_${arch}_deb.deb" "${out}/redoma_$2_${arch}.deb"; then
			echo "FAIL: $1: redoma_$2_${arch}.deb is not the ${arch} package" >&2
			fail=1
		fi
	done
}
refused() { # <deb version> <why>
	local out="${dir}/out$((n += 1))"
	if bash "${script}" "$1" "${dir}/bin" "${out}" >/dev/null 2>&1; then
		echo "FAIL: '$1', $2: accepted, want it refused" >&2
		fail=1
	fi
	if [[ -e ${out} ]]; then
		echo "FAIL: '$1', $2: wrote ${out}" >&2
		fail=1
	fi
}

expect 0.1.0 0.1.0
expect 0.1.0~rc.1 0.1.0-rc.1
expect 0.1.0~rc.10 0.1.0-rc.10
expect 0.1.0~rc.1.git20261030120000.abcdef1 0.1.0-rc.1.git20261030120000.abcdef1
expect 0.1.0~git20261030120000.abcdef1.dirty 0.1.0-git20261030120000.abcdef1.dirty
refused "" "no version"
refused 0.1.0+deb1 "+ is not a kept character"
refused 1:0.1.0 "neither is :"
refused "0.1.0 rc" "nor a space"
refused "0.1.0/rc" "a path"

# A missing package fails.
if bash "${script}" 0.1.0 "${dir}/nothing" "${dir}/missing" >/dev/null 2>&1; then
	echo "FAIL: no packages: accepted" >&2
	fail=1
fi

[ "${fail}" = 0 ]
echo "release assets as expected"
