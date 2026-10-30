#!/usr/bin/env bash
# Each Claude Code tarball MODULE.bazel pins by sha256 is the version it pins
# by name, for the architecture it was fetched for: its package.json says so.
#
# Arguments: the runfiles paths of the repository's version.bzl, then of the
# x64 and the arm64 tarballs' package.json.
set -euo pipefail

version_bzl="${TEST_SRCDIR}/$1"
want="$(sed -n 's/^CLAUDE_CODE_VERSION = "\(.*\)"$/\1/p' "${version_bzl}")"
if [[ -z "${want}" ]]; then
	echo "FAIL: no CLAUDE_CODE_VERSION in ${version_bzl}" >&2
	exit 1
fi

check() { # <npm arch> <package.json>
	local arch=$1 package_json="${TEST_SRCDIR}/$2" got name
	got="$(sed -n 's/^ *"version": *"\([^"]*\)".*$/\1/p' "${package_json}" | head -1)"
	name="$(sed -n 's/^ *"name": *"\([^"]*\)".*$/\1/p' "${package_json}" | head -1)"
	if [[ "${name}" != "@anthropic-ai/claude-code-linux-${arch}" ]]; then
		echo "FAIL: the ${arch} tarball is ${name}, not @anthropic-ai/claude-code-linux-${arch}" >&2
		exit 1
	fi
	if [[ "${got}" != "${want}" ]]; then
		echo "FAIL: the ${arch} tarball is Claude Code ${got}; MODULE.bazel pins ${want}: change its sha256 with its version" >&2
		exit 1
	fi
	echo "Claude Code ${got}, linux-${arch}"
}

check x64 "$2"
check arm64 "$3"
