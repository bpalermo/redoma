#!/usr/bin/env bash
# The redoma packages: their control fields, exactly their files with their
# modes and owners, the conffiles, a binary of each package's architecture,
# the stamped version, and the packaged postinst's check of the services'
# ids.
#
# Arguments: the runfiles paths of the amd64 and arm64 packages and of the
# version file they were built with.
set -euo pipefail

amd64="${TEST_SRCDIR}/$1"
arm64="${TEST_SRCDIR}/$2"
version="$(cat "${TEST_SRCDIR}/$3")"
work="${TEST_TMPDIR}/deb"
mkdir -p "${work}"

fail() {
	echo "FAIL: $*" >&2
	exit 1
}

# The files every package holds, as `dpkg-deb -c` lists them: mode, owner,
# path (sizes and dates left out).
want_files() {
	cat <<'EOF'
drwxr-xr-x root/root etc/
drwxr-xr-x root/root etc/containers/
drwxr-xr-x root/root etc/containers/containers.conf.d/
-rw-r--r-- root/root etc/containers/containers.conf.d/50-redoma-hooks.conf
drwxr-xr-x root/root etc/redoma/
-rw-r--r-- root/root etc/redoma/redoma.conf
drwxr-xr-x root/root usr/
drwxr-xr-x root/root usr/bin/
-rwxr-xr-x root/root usr/bin/redoma
drwxr-xr-x root/root usr/lib/
drwxr-xr-x root/root usr/lib/systemd/
drwxr-xr-x root/root usr/lib/systemd/system/
-rw-r--r-- root/root usr/lib/systemd/system/redoma-relay.service
-rw-r--r-- root/root usr/lib/systemd/system/redoma-xds.service
drwxr-xr-x root/root usr/lib/sysusers.d/
-rw-r--r-- root/root usr/lib/sysusers.d/redoma.conf
drwxr-xr-x root/root usr/share/
drwxr-xr-x root/root usr/share/containers/
drwxr-xr-x root/root usr/share/containers/oci/
drwxr-xr-x root/root usr/share/containers/oci/hooks.d/
-rw-r--r-- root/root usr/share/containers/oci/hooks.d/redoma-create-runtime.json
-rw-r--r-- root/root usr/share/containers/oci/hooks.d/redoma-poststop.json
drwxr-xr-x root/root usr/share/doc/
drwxr-xr-x root/root usr/share/doc/redoma/
-rw-r--r-- root/root usr/share/doc/redoma/NOTICE
-rw-r--r-- root/root usr/share/doc/redoma/README.md
-rw-r--r-- root/root usr/share/doc/redoma/copyright
drwxr-xr-x root/root usr/share/doc/redoma/examples/
-rw-r--r-- root/root usr/share/doc/redoma/examples/anthropic-key.yaml
-rw-r--r-- root/root usr/share/doc/redoma/examples/envoy.yaml
EOF
}

# elf_machine prints a file's ELF e_machine, little-endian, as a number:
# 62 x86-64, 183 aarch64.
elf_machine() {
	od -An -t u2 -j 18 -N 2 "$1" | tr -d ' '
}

check_package() { # <deb> <arch> <e_machine>
	local deb=$1 arch=$2 machine=$3 dir="${work}/$2"
	mkdir -p "${dir}"
	[[ $(dpkg-deb -f "${deb}" Package) == redoma ]] || fail "${arch}: Package"
	[[ $(dpkg-deb -f "${deb}" Architecture) == "${arch}" ]] || fail "${arch}: Architecture $(dpkg-deb -f "${deb}" Architecture)"
	[[ $(dpkg-deb -f "${deb}" Maintainer) == "Bruno Palermo <b@palermo.dev>" ]] || fail "${arch}: Maintainer"
	[[ $(dpkg-deb -f "${deb}" Depends) == systemd ]] || fail "${arch}: Depends $(dpkg-deb -f "${deb}" Depends)"
	[[ $(dpkg-deb -f "${deb}" Version) == "${version}" ]] || fail "${arch}: Version $(dpkg-deb -f "${deb}" Version), not ${version}"
	[[ -z $(dpkg-deb -f "${deb}" License) ]] || fail "${arch}: a License control field"

	dpkg-deb -c "${deb}" | awk '{print $1, $2, $6}' | sort >"${dir}/files"
	want_files | sort >"${dir}/want"
	diff -u "${dir}/want" "${dir}/files" || fail "${arch}: the files differ (-want +got)"

	dpkg-deb -e "${deb}" "${dir}/control"
	printf '%s\n' /etc/containers/containers.conf.d/50-redoma-hooks.conf /etc/redoma/redoma.conf >"${dir}/conffiles.want"
	sort "${dir}/control/conffiles" | diff -u "${dir}/conffiles.want" - || fail "${arch}: conffiles"
	for script in postinst prerm postrm; do
		[[ -x "${dir}/control/${script}" ]] || fail "${arch}: no executable ${script}"
	done

	dpkg-deb -x "${deb}" "${dir}/root"
	[[ $(elf_machine "${dir}/root/usr/bin/redoma") == "${machine}" ]] ||
		fail "${arch}: /usr/bin/redoma is ELF machine $(elf_machine "${dir}/root/usr/bin/redoma"), not ${machine}"
	cmp -s "${dir}/root/usr/lib/sysusers.d/redoma.conf" "${TEST_SRCDIR}/_main/deb/sysusers.conf" || fail "${arch}: sysusers.d differs from deb/sysusers.conf"
}

check_package "${amd64}" amd64 62
check_package "${arm64}" arm64 183

# The version is VERSION's, a candidate's or a snapshot's (deb_version.sh).
[[ ${version} =~ ^[0-9]+\.[0-9]+\.[0-9]+(~rc\.[0-9]+|~(rc\.[0-9]+\.)?git[0-9]{14}\.[0-9a-f]{7}(\.dirty)?|~dev)?$ ]] || fail "version ${version}"
# The copyright file is machine-readable (DEP-5) with a copyright line.
grep -qx 'Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/' "${work}/amd64/root/usr/share/doc/redoma/copyright" || fail "copyright: not DEP-5"
grep -qx 'Copyright: 2026 Bruno Palermo <b@palermo.dev>' "${work}/amd64/root/usr/share/doc/redoma/copyright" || fail "copyright: no copyright line"

# The postinst the package carries.
postinst="${work}/amd64/control/postinst"

# The postinst against fake ids: getent answers from a passwd and a group
# file, systemd-sysusers and systemctl do nothing.
fake="${work}/fake"
mkdir -p "${fake}/bin"
cat >"${fake}/bin/getent" <<'EOF'
#!/bin/sh
grep "^$2:" "${FAKE}/$1"
EOF
printf '#!/bin/sh\nexit 0\n' >"${fake}/bin/systemd-sysusers"
printf '#!/bin/sh\nexit 0\n' >"${fake}/bin/systemctl"
chmod +x "${fake}/bin/"*

run_postinst() { # <passwd> <group>: the postinst's output; fails as it fails
	printf '%s\n' "$1" >"${fake}/passwd"
	printf '%s\n' "$2" >"${fake}/group"
	FAKE="${fake}" PATH="${fake}/bin:${PATH}" sh "${postinst}" configure 2>&1
}

good_passwd='redoma-sds:x:961:961::/:/usr/sbin/nologin
redoma-relay:x:963:963::/:/usr/sbin/nologin'
good_group='redoma-sds:x:961:
redoma-relay:x:963:
redoma-lead:x:964:'
out=$(run_postinst "${good_passwd}" "${good_group}") || fail "postinst with the right ids: ${out}"

if out=$(run_postinst "${good_passwd/redoma-relay:x:963/redoma-relay:x:1963}" "${good_group}"); then
	fail "postinst accepted redoma-relay at uid 1963"
fi
[[ ${out} == *"the user redoma-relay has id '1963', not 963"* ]] || fail "postinst's refusal does not name the user: ${out}"

if out=$(run_postinst "${good_passwd}" "${good_group/redoma-lead:x:964:/}"); then
	fail "postinst accepted a missing redoma-lead group"
fi
[[ ${out} == *"the group redoma-lead has id '', not 964"* ]] || fail "postinst's refusal does not name the group: ${out}"

echo "redoma_amd64.deb and redoma_arm64.deb are as expected"
