#!/bin/bash
# Builds the initramfs: naru-init as /init, mvmd and mvmd-supervisor from the pinned mvmd commit,
# the Firecracker binary mvmd's fetch.sh verifies, and the stand-in service the tests configure.
# Every program is static (CGO_ENABLED=0). The archive is a newc cpio written by the kernel's own
# usr/gen_init_cpio, which takes the entries in the order of initramfs.list, owns them by 0:0 and
# stamps every mtime with SOURCE_DATE_EPOCH; it is compressed with gzip -9n, which stores no name
# or time. The same inputs therefore give the same bytes (image/build.env fixes the environment).
#
# Usage: build.sh <build-dir> <kernel-build-dir> <output-dir>
# <build-dir> receives the mvmd checkout and the built programs, <kernel-build-dir> is the one
# image/kernel/build.sh was given, whose dl/ holds the verified kernel tarball (gen_init_cpio.c is
# taken from it), and <output-dir> receives initramfs.cpio.gz and, for part B, the files mvmd's
# fetch.sh yields: vmlinux, the guest kernel, and inputs.env.
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <build-dir> <kernel-build-dir> <output-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
mkdir -p "$1" "$3"
build=$(cd "$1" && pwd)
out=$(cd "$3" && pwd)
# shellcheck source=pin.env
. "$here/pin.env"
# shellcheck source=../build.env
. "$here/../build.env"
# shellcheck source=../kernel/pin.env
. "$here/../kernel/pin.env"
tarball=$(cd "$2" && pwd)/dl/${KERNEL_URL##*/}

if [ "$(sha256sum "$tarball" | cut -d' ' -f1)" != "$KERNEL_SHA256" ]; then
  echo "initramfs: $tarball does not have the pinned kernel SHA-256" >&2
  exit 1
fi

bin=$build/bin
rm -rf "$bin"
mkdir -p "$bin"

# naru-init and the stand-in are built from this repository.
gobuild() { # <module-dir> <output> <package>
  (cd "$1" && CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$2" "$3")
}
gobuild "$root/init" "$bin/naru-init" ./cmd/naru-init
gobuild "$root/tooling/qemu" "$bin/naru-standin" ./cmd/naru-standin

# mvmd is fetched at its pinned commit and nowhere else.
src=$build/mvmd
if [ ! -d "$src/.git" ] || [ "$(git -C "$src" rev-parse HEAD 2>/dev/null)" != "$MVMD_COMMIT" ]; then
  rm -rf "$src"
  git init --quiet "$src"
  git -C "$src" fetch --quiet --depth 1 "$MVMD_REPO" "$MVMD_COMMIT"
  git -C "$src" checkout --quiet --detach FETCH_HEAD
fi
got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$MVMD_COMMIT" ]; then
  echo "initramfs: the mvmd checkout is at $got, not the pinned $MVMD_COMMIT" >&2
  exit 1
fi
(cd "$src" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$bin/mvmd" ./daemon/cmd/mvmd)
(cd "$src" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$bin/mvmd-supervisor" ./daemon/cmd/mvmd-supervisor)

# Firecracker, and the guest kernel for part B, from mvmd's own digest-checked fetch.
"$src/tooling/firecracker/fetch.sh" "$build/inputs" >/dev/null
cp "$build/inputs/firecracker" "$bin/firecracker"
install -m 0644 "$build/inputs/vmlinux" "$out/vmlinux"
sed "s#=$build/inputs/#=$out/#" "$build/inputs/inputs.env" >"$out/inputs.env"

for f in naru-init mvmd mvmd-supervisor firecracker naru-standin; do
  if ! file "$bin/$f" | grep -Eq 'statically linked|static-pie linked'; then
    echo "initramfs: $f is not a static executable: $(file "$bin/$f")" >&2
    exit 1
  fi
done

# gen_init_cpio is the kernel's own, compiled from the pinned tarball.
tools=$build/tools
mkdir -p "$tools"
tar -xf "$tarball" -C "$tools" --strip-components=2 "linux-$KERNEL_VERSION/usr/gen_init_cpio.c"
gcc -O2 -o "$tools/gen_init_cpio" "$tools/gen_init_cpio.c"

sed "s#@OUT@#$bin#" "$here/initramfs.list" >"$build/initramfs.list"
"$tools/gen_init_cpio" -t "$SOURCE_DATE_EPOCH" "$build/initramfs.list" >"$build/initramfs.cpio"
gzip -9n <"$build/initramfs.cpio" >"$out/initramfs.cpio.gz"
echo "initramfs: built $out/initramfs.cpio.gz, SHA-256 $(sha256sum "$out/initramfs.cpio.gz" | cut -d' ' -f1)"
