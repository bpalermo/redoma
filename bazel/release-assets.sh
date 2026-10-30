#!/usr/bin/env bash
# The files a release uploads (.github/workflows/release.yml, the `build`
# job): the packages, named redoma_<asset version>_<arch>.deb, and their
# SHA256SUMS, which lists exactly those names.
#
# GitHub renames an uploaded asset whose name has a character it does not
# keep (it turns `~` into `.`), or a leading or trailing period, and the
# downloaded files would no longer match SHA256SUMS. The asset version is
# therefore the packages' version with `~` as `-`: 0.1.0~rc.1 is uploaded
# as redoma_0.1.0-rc.1_amd64.deb, the tag's spelling, while the package
# inside keeps its control Version, 0.1.0~rc.1, which dpkg orders by. Any
# name with another character than [A-Za-z0-9._-], or a period at either
# end, fails here, before anything is published.
#
# Arguments: the packages' version, the directory with the built
# redoma_<arch>_deb.deb, and the directory to write the assets to.
set -euo pipefail

deb_version=$1
src=$2
out=$3
archs=(amd64 arm64)

# A name GitHub uploads unchanged.
kept() {
	[[ $1 =~ ^[A-Za-z0-9_-]([A-Za-z0-9._-]*[A-Za-z0-9_-])?$ ]]
}

if [[ -z ${deb_version} ]]; then
	echo "::error::no package version" >&2
	exit 1
fi
asset_version="${deb_version//\~/-}"

names=()
for arch in "${archs[@]}"; do
	names+=("redoma_${asset_version}_${arch}.deb")
done
names+=(SHA256SUMS)
for name in "${names[@]}"; do
	if ! kept "${name}"; then
		echo "::error::GitHub would rename the asset ${name}: only [A-Za-z0-9._-], and no period at either end" >&2
		exit 1
	fi
done

mkdir -p "${out}"
debs=()
for arch in "${archs[@]}"; do
	name="redoma_${asset_version}_${arch}.deb"
	cp "${src}/redoma_${arch}_deb.deb" "${out}/${name}"
	debs+=("${name}")
done
# The sums are of the files under the names they are uploaded as.
(cd "${out}" && sha256sum -- "${debs[@]}" >SHA256SUMS)
cat "${out}/SHA256SUMS"
