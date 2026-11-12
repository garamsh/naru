#!/bin/bash
# Builds the host kernel from the pinned tarball: fetches it, verifies its SHA-256, merges
# naru.config over x86_64 defconfig, fails unless every line of required.txt matches the final
# .config, and builds bzImage. The source is unpacked and the object directory emptied before each
# build, so the same inputs give the same bzImage (image/build.env fixes what the kernel stamps
# into it). A build whose inputs match the last one is skipped.
# The result is bzImage.bare, a kernel with no initramfs; embed.sh puts the initramfs into it and
# writes bzImage (ADR 0020).
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> keeps the download in dl/ and receives the source and the object directory;
# <output-dir> receives bzImage.bare and kernel.config, the final configuration.
set -euo pipefail

# A VERSION=<name> given to the outer make (make release) reaches this make through MAKEFLAGS and
# breaks the kernel's own VERSION; the builds below take no variable from the caller.
unset MAKEFLAGS MFLAGS

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <build-dir> <output-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$1" "$2"
build=$(cd "$1" && pwd)
out=$(cd "$2" && pwd)
# shellcheck source=pin.env
. "$here/pin.env"
# shellcheck source=../build.env
. "$here/../build.env"

stamp=$(cat "$here/pin.env" "$here/naru.config" "$here/required.txt" "$here/build.sh" "$here/../build.env" | sha256sum | cut -d' ' -f1)
if [ -f "$out/bzImage.bare" ] && [ -f "$build/obj/.config" ] && [ -f "$out/kernel.stamp" ] && [ "$(cat "$out/kernel.stamp")" = "$stamp" ]; then
  echo "kernel: $out/bzImage.bare is up to date with its inputs"
  exit 0
fi

tarball=$build/dl/${KERNEL_URL##*/}
mkdir -p "$build/dl"
if [ ! -f "$tarball" ] || [ "$(sha256sum "$tarball" | cut -d' ' -f1)" != "$KERNEL_SHA256" ]; then
  rm -f "$tarball" "$tarball.part"
  curl --fail --silent --show-error --location --connect-timeout 30 --retry 4 --retry-all-errors \
    --output "$tarball.part" "$KERNEL_URL"
  mv "$tarball.part" "$tarball"
fi
got=$(sha256sum "$tarball" | cut -d' ' -f1)
if [ "$got" != "$KERNEL_SHA256" ]; then
  echo "kernel: $tarball has SHA-256 $got, not the pinned $KERNEL_SHA256" >&2
  exit 1
fi
echo "kernel: $tarball has the pinned SHA-256 $KERNEL_SHA256"

src=$build/linux-$KERNEL_VERSION
obj=$build/obj
rm -rf "$src" "$obj"
mkdir -p "$obj"
tar -xf "$tarball" -C "$build"

make -C "$src" O="$obj" ARCH=x86_64 defconfig >/dev/null
(cd "$src" && scripts/kconfig/merge_config.sh -m -O "$obj" "$obj/.config" "$here/naru.config" >/dev/null)
make -C "$src" O="$obj" ARCH=x86_64 olddefconfig >/dev/null

# kconfig drops an option whose dependencies are unmet and exits zero, so what the build needs is
# read back from the file it will build with.
missing=0
while IFS= read -r line; do
  case "$line" in '' | '##'*) continue ;; esac
  if ! grep -Eqx -e "$line" "$obj/.config"; then
    echo "kernel: the final .config has no line matching: $line" >&2
    missing=$((missing + 1))
  fi
done <"$here/required.txt"
if [ "$missing" -ne 0 ]; then
  echo "kernel: $missing required option(s) missing from the final .config" >&2
  exit 1
fi
echo "kernel: the final .config meets all $(grep -vc -e '^##' -e '^$' "$here/required.txt") lines of required.txt"

make -C "$src" O="$obj" ARCH=x86_64 -j"$(nproc)" bzImage >"$build/build.log" 2>&1 || {
  tail -n 40 "$build/build.log" >&2
  echo "kernel: the build failed; the full log is $build/build.log" >&2
  exit 1
}
install -m 0644 "$obj/arch/x86/boot/bzImage" "$out/bzImage.bare"
install -m 0644 "$obj/.config" "$out/kernel.config"
echo "$stamp" >"$out/kernel.stamp"
echo "kernel: built $out/bzImage.bare, SHA-256 $(sha256sum "$out/bzImage.bare" | cut -d' ' -f1)"
