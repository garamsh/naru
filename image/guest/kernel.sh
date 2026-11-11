#!/bin/bash
# Builds one guest kernel from the host kernel's pinned tarball (ADR 0010): merges
# kernels/<name>/naru.config over x86_64 tinyconfig, fails unless every line of
# kernels/<name>/required.txt matches the final .config, and builds the uncompressed ELF vmlinux,
# which has a PVH entry because CONFIG_PVH is required. The source is unpacked and the object
# directory emptied before each build (image/build.env fixes what the kernel stamps into it).
#
# Usage: kernel.sh <name> <kernel-build-dir> <build-dir> <output-dir>
# <kernel-build-dir> is the one image/kernel/build.sh was given, whose dl/ holds the verified
# tarball; <build-dir> receives the source and the object directory; <output-dir> receives
# vmlinux and kernel.config, the final configuration.
set -euo pipefail

# A VERSION=<name> given to the outer make (make release) reaches this make through MAKEFLAGS and
# breaks the kernel's own VERSION; the builds below take no variable from the caller.
unset MAKEFLAGS MFLAGS

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <name> <kernel-build-dir> <build-dir> <output-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
name=$1
if ! [[ "$name" =~ ^[a-z0-9-]{1,63}$ ]]; then
  echo "guest kernel: $name is not 1 to 63 characters of [a-z0-9-]" >&2
  exit 1
fi
kdir=$here/kernels/$name
mkdir -p "$3" "$4"
build=$(cd "$3" && pwd)
out=$(cd "$4" && pwd)
# shellcheck source=../kernel/pin.env
. "$here/../kernel/pin.env"
# shellcheck source=../build.env
. "$here/../build.env"

stamp=$(cat "$here/../kernel/pin.env" "$kdir/naru.config" "$kdir/required.txt" "$here/kernel.sh" "$here/../build.env" | sha256sum | cut -d' ' -f1)
if [ -f "$out/vmlinux" ] && [ -f "$out/kernel.stamp" ] && [ "$(cat "$out/kernel.stamp")" = "$stamp" ]; then
  echo "guest kernel $name: $out/vmlinux is up to date with its inputs"
  exit 0
fi

tarball=$(cd "$2" && pwd)/dl/${KERNEL_URL##*/}
if [ "$(sha256sum "$tarball" | cut -d' ' -f1)" != "$KERNEL_SHA256" ]; then
  echo "guest kernel: $tarball does not have the pinned kernel SHA-256" >&2
  exit 1
fi

src=$build/linux-$KERNEL_VERSION
obj=$build/obj
rm -rf "$src" "$obj"
mkdir -p "$obj"
tar -xf "$tarball" -C "$build"

make -C "$src" O="$obj" ARCH=x86_64 tinyconfig >/dev/null
(cd "$src" && scripts/kconfig/merge_config.sh -m -O "$obj" "$obj/.config" "$kdir/naru.config" >/dev/null)
make -C "$src" O="$obj" ARCH=x86_64 olddefconfig >/dev/null

# kconfig drops an option whose dependencies are unmet and exits zero, so what the build needs is
# read back from the file it will build with.
missing=0
while IFS= read -r line; do
  case "$line" in '' | '##'*) continue ;; esac
  if ! grep -Eqx -e "$line" "$obj/.config"; then
    echo "guest kernel $name: the final .config has no line matching: $line" >&2
    missing=$((missing + 1))
  fi
done <"$kdir/required.txt"
if [ "$missing" -ne 0 ]; then
  echo "guest kernel $name: $missing required option(s) missing from the final .config" >&2
  exit 1
fi
echo "guest kernel $name: the final .config meets all $(grep -vc -e '^##' -e '^$' "$kdir/required.txt") lines of required.txt"

make -C "$src" O="$obj" ARCH=x86_64 -j"$(nproc)" vmlinux >"$build/build.log" 2>&1 || {
  tail -n 40 "$build/build.log" >&2
  echo "guest kernel $name: the build failed; the full log is $build/build.log" >&2
  exit 1
}
install -m 0644 "$obj/vmlinux" "$out/vmlinux"
install -m 0644 "$obj/.config" "$out/kernel.config"
echo "$stamp" >"$out/kernel.stamp"
echo "guest kernel $name: built $out/vmlinux, SHA-256 $(sha256sum "$out/vmlinux" | cut -d' ' -f1)"
