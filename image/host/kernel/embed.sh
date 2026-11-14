#!/bin/bash
# Puts the initramfs into the kernel: sets CONFIG_INITRAMFS_SOURCE to the finished initramfs.cpio.gz
# in the object directory image/host/kernel/build.sh left, builds bzImage again (only the initramfs
# object and the link change), and writes it as <output-dir>/bzImage. That one file is the whole
# system the firmware loads, and a slot of the ESP is that file (ADR 0020). The kernel's EFI stub
# concatenates every initrd= file it is given, so a separate initramfs could not be chosen per slot.
# A build whose inputs match the last one is skipped.
#
# Usage: embed.sh <build-dir> <output-dir> <initramfs.cpio.gz>
# <build-dir> and <output-dir> are the ones build.sh was given.
set -euo pipefail

unset MAKEFLAGS MFLAGS

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <build-dir> <output-dir> <initramfs.cpio.gz>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
build=$(cd "$1" && pwd)
out=$(cd "$2" && pwd)
initramfs=$(cd "$(dirname "$3")" && pwd)/$(basename "$3")
# shellcheck source=../../common/kernel/pin.env
. "$here/../../common/kernel/pin.env"
# shellcheck source=../../build.env
. "$here/../../build.env"

# kbuild uses a single file named *.cpio.* as it is and compresses nothing again.
case "$initramfs" in *.cpio.gz) ;; *)
  echo "embed: $initramfs is not a .cpio.gz" >&2
  exit 1
  ;;
esac
for f in "$out/bzImage.bare" "$out/kernel.stamp" "$build/obj/.config" "$initramfs"; do
  if [ ! -f "$f" ]; then
    echo "embed: $f is missing; image/host/kernel/build.sh and image/host/initramfs/build.sh write it" >&2
    exit 1
  fi
done

stamp=$(
  cat "$out/kernel.stamp" "$here/embed.sh"
  sha256sum "$initramfs" | cut -d' ' -f1
)
stamp=$(echo "$stamp" | sha256sum | cut -d' ' -f1)
if [ -f "$out/bzImage" ] && [ -f "$out/bzImage.stamp" ] && [ "$(cat "$out/bzImage.stamp")" = "$stamp" ]; then
  echo "embed: $out/bzImage is up to date with its inputs"
  exit 0
fi

src=$build/linux-$KERNEL_VERSION
obj=$build/obj
"$src/scripts/config" --file "$obj/.config" --set-str INITRAMFS_SOURCE "$initramfs"
make -C "$src" O="$obj" ARCH=x86_64 olddefconfig >/dev/null
grep -Fqx "CONFIG_INITRAMFS_SOURCE=\"$initramfs\"" "$obj/.config" || {
  echo "embed: the final .config does not name $initramfs as CONFIG_INITRAMFS_SOURCE" >&2
  exit 1
}
make -C "$src" O="$obj" ARCH=x86_64 -j"$(nproc)" bzImage >"$build/embed.log" 2>&1 || {
  tail -n 40 "$build/embed.log" >&2
  echo "embed: the build failed; the full log is $build/embed.log" >&2
  exit 1
}
install -m 0644 "$obj/arch/x86/boot/bzImage" "$out/bzImage"
echo "$stamp" >"$out/bzImage.stamp"
echo "embed: built $out/bzImage with the initramfs inside, SHA-256 $(sha256sum "$out/bzImage" | cut -d' ' -f1)"
