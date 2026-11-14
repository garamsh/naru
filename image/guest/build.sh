#!/bin/bash
# Builds what a Machine boots on and writes the manifest that names it (ADR 0010): each guest
# kernel kernels.env declares, and the kernel of the guest form's boot set, with kernel.sh, and the two initrds, the Guest Agent's and the
# converter's, with mvmd's own Makefile in the mvmd checkout; it checks each initrd against the
# SHA-256 of pin.env. image/initramfs/build.sh puts the result into the initramfs, so these are
# parts of the image and not inputs a test supplies.
#
# Usage: build.sh <mvmd-checkout> <kernel-build-dir> <build-dir> <output-dir>
# <kernel-build-dir> is the one image/kernel/build.sh was given, whose dl/ holds the verified
# kernel tarball; <build-dir> receives the guest kernels' sources; <output-dir> receives
# kernels/<name>/vmlinux, boot-sets/<boot set>/vmlinux, guest-initrd.cpio, converter-initrd.cpio and guest.json, which names each
# with the path it has inside the image and its SHA-256. The Rust toolchain is the one mvmd's
# rust-toolchain.toml names, which rustup installs for the user, and erofs-utils builds itself.
# Nothing is built again when its inputs and the initrds' digests are unchanged.
set -euo pipefail

# A VERSION=<name> given to the outer make (make release) reaches this make through MAKEFLAGS and
# breaks the kernel's own VERSION; the builds below take no variable from the caller.
unset MAKEFLAGS MFLAGS

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <mvmd-checkout> <kernel-build-dir> <build-dir> <output-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
src=$(cd "$1" && pwd)
mkdir -p "$3" "$4"
build=$(cd "$3" && pwd)
out=$(cd "$4" && pwd)
# shellcheck source=pin.env
. "$here/pin.env"
# shellcheck source=kernels.env
. "$here/kernels.env"
# shellcheck source=../initramfs/pin.env
. "$here/../initramfs/pin.env"

got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$MVMD_COMMIT" ]; then
  echo "guest: the mvmd checkout is at $got, not the pinned $MVMD_COMMIT" >&2
  exit 1
fi

kernel_listed=0
for name in $GUEST_KERNELS; do
  [ "$name" = "$GUEST_DEFAULT_KERNEL" ] && kernel_listed=1
done
if [ "$kernel_listed" -ne 1 ]; then
  echo "guest: GUEST_DEFAULT_KERNEL $GUEST_DEFAULT_KERNEL is not one of GUEST_KERNELS ($GUEST_KERNELS)" >&2
  exit 1
fi

if ! [[ "$GUEST_BOOT_SET" =~ ^[a-z0-9-]{1,63}$ ]]; then
  echo "guest: GUEST_BOOT_SET $GUEST_BOOT_SET is not 1 to 63 characters of [a-z0-9-]" >&2
  exit 1
fi

sha_of() { sha256sum "$1" | cut -d' ' -f1; }
digest_is() { [ -f "$1" ] && [ "$(sha_of "$1")" = "$2" ]; }

for name in $GUEST_KERNELS; do
  "$here/kernel.sh" "$name" "$2" "$build/kernels/$name" "$out/kernels/$name"
done

# The guest form's kernel, for the boot set image/initramfs/build.sh completes with its initrd.
"$here/kernel.sh" "$GUEST_BOOT_SET_KERNEL" "$2" "$build/boot-sets/$GUEST_BOOT_SET" "$out/boot-sets/$GUEST_BOOT_SET"

if ! digest_is "$out/guest-initrd.cpio" "$GUEST_INITRD_SHA256" || ! digest_is "$out/converter-initrd.cpio" "$CONVERTER_INITRD_SHA256"; then
  make -C "$src" guest-initrd converter-initrd
  cp "$src/guest-agent/target/initrd.cpio" "$out/guest-initrd.cpio"
  cp "$src/converter/target/initrd.cpio" "$out/converter-initrd.cpio"
fi
for pair in "guest-initrd.cpio:$GUEST_INITRD_SHA256" "converter-initrd.cpio:$CONVERTER_INITRD_SHA256"; do
  if ! digest_is "$out/${pair%%:*}" "${pair#*:}"; then
    echo "guest: $out/${pair%%:*} has SHA-256 $(sha_of "$out/${pair%%:*}"), not the pinned ${pair#*:}" >&2
    exit 1
  fi
  echo "guest: ${pair%%:*} has the pinned SHA-256 ${pair#*:}"
done

# guest.json: the kernels as the `kernels` map and default_kernel of mvmd's driver configuration
# (garamsh/mvmd#364), and the two initrds, each with its path inside the image. The digests are
# read back from the files written above.
{
  printf '{\n  "kernels": {'
  sep=
  for name in $GUEST_KERNELS; do
    printf '%s\n    "%s": {"path": "%s/kernels/%s/vmlinux", "sha256": "%s"}' \
      "$sep" "$name" "$GUEST_IMAGE_DIR" "$name" "$(sha_of "$out/kernels/$name/vmlinux")"
    sep=,
  done
  printf '\n  },\n  "default_kernel": "%s",\n' "$GUEST_DEFAULT_KERNEL"
  printf '  "guest_initrd": {"path": "%s/guest-initrd.cpio", "sha256": "%s"},\n' "$GUEST_IMAGE_DIR" "$(sha_of "$out/guest-initrd.cpio")"
  printf '  "converter_initrd": {"path": "%s/converter-initrd.cpio", "sha256": "%s"}\n}\n' "$GUEST_IMAGE_DIR" "$(sha_of "$out/converter-initrd.cpio")"
} >"$out/guest.json.part"
mv "$out/guest.json.part" "$out/guest.json"
echo "guest: wrote $out/guest.json"
