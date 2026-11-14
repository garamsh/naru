#!/bin/bash
# Builds the Node's kernel as a microVM (ADR 0034, 0054): the common kernel configuration with this
# directory's overlay over x86_64 defconfig, built by image/common/kernel/kernel.sh into an
# uncompressed vmlinux, the kernel of the boot set boot-set.env names.
#
# Usage: build.sh <kernel-build-dir> <build-dir> <guest-dir>
# <kernel-build-dir> is the one image/host/kernel/build.sh was given, whose dl/ holds the verified
# kernel tarball; <build-dir> receives the source and the object directory; <guest-dir> receives
# boot-sets/<boot set>/vmlinux and kernel.config, beside the guest files image/host/guest/build.sh
# writes there.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <kernel-build-dir> <build-dir> <guest-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=../boot-set.env
. "$here/../boot-set.env"
if ! [[ "$GUEST_BOOT_SET" =~ ^[a-z0-9-]{1,63}$ ]]; then
  echo "node kernel: GUEST_BOOT_SET $GUEST_BOOT_SET is not 1 to 63 characters of [a-z0-9-]" >&2
  exit 1
fi
"$here/../../common/kernel/kernel.sh" "$here" "$1" "$2/boot-sets/$GUEST_BOOT_SET" "$3/boot-sets/$GUEST_BOOT_SET"
