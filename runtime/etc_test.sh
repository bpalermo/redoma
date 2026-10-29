#!/usr/bin/env bash
# The /etc files of the runtime image's etc_layer: hosts are looked up in
# /etc/hosts before DNS, and /etc/hosts has localhost. (image_test checks that
# both are in the image; rules_img's structure test reads no file contents.)
#
# Arguments: the runfiles paths of nsswitch.conf and hosts.
set -euo pipefail

nsswitch="${TEST_SRCDIR}/$1"
hosts="${TEST_SRCDIR}/$2"

if ! grep -Eq '^hosts:[[:space:]]+files dns$' "${nsswitch}"; then
	echo "FAIL: nsswitch.conf has no 'hosts: files dns' line" >&2
	exit 1
fi
if ! grep -Eq '^127\.0\.0\.1[[:space:]]+localhost$' "${hosts}"; then
	echo "FAIL: hosts has no '127.0.0.1 localhost' line" >&2
	exit 1
fi
echo "nsswitch.conf and hosts are as expected"
