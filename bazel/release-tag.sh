#!/usr/bin/env bash
# Which release a tag publishes (.github/workflows/release.yml, the `build`
# job): v<VERSION> is the release, v<VERSION>-rc.<n> a candidate, n from 1
# and without a leading zero, as bazel/workspace_status.sh and
# deb/deb_version.sh read them. Any other tag fails: it would publish
# packages that disagree with their release.
#
# Arguments: the VERSION file, then the tag. Printed, and appended to
# GITHUB_OUTPUT when it is set:
#   deb         the packages' version: <VERSION>, or <VERSION>~rc.<n>
#   prerelease  true for a candidate
set -euo pipefail

version="$(tr -d '[:space:]' <"$1")"
tag=$2

if [[ ${tag} == "v${version}" ]]; then
	deb="${version}"
	prerelease=false
elif [[ ${tag} =~ ^v${version//./\\.}-rc\.([1-9][0-9]*)$ ]]; then
	deb="${version}~rc.${BASH_REMATCH[1]}"
	prerelease=true
else
	echo "::error::tag ${tag}, but VERSION says v${version} or v${version}-rc.<n> (n from 1)" >&2
	exit 1
fi

out="deb=${deb}
prerelease=${prerelease}"
echo "${out}"
if [[ -n ${GITHUB_OUTPUT:-} ]]; then
	echo "${out}" >>"${GITHUB_OUTPUT}"
fi
