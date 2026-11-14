#!/bin/bash
# Builds mke2fs, statically linked, from the pinned e2fsprogs tarball (ADR 0011): fetches it,
# verifies its SHA-256, configures an out-of-tree build with what mke2fs does not need switched
# off, builds the libraries and mke2fs only, strips it and fails unless it is a static executable.
# The source is unpacked and the object directory emptied before each build; the build paths are
# mapped away and nothing is stamped, so the same inputs give the same bytes (image/build.env fixes
# the environment). A build whose inputs match the last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> keeps the download in dl/ and receives the source and the object directory;
# <output-dir> receives mke2fs.
set -euo pipefail

# A VERSION=<name> given to the outer make (make release) reaches this make through MAKEFLAGS; the
# builds below take no variable from the caller.
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
# shellcheck source=../../build.env
. "$here/../../build.env"

stamp=$(cat "$here/pin.env" "$here/build.sh" "$here/../../build.env" | sha256sum | cut -d' ' -f1)
if [ -f "$out/mke2fs" ] && [ -f "$out/e2fsprogs.stamp" ] && [ "$(cat "$out/e2fsprogs.stamp")" = "$stamp" ]; then
  echo "e2fsprogs: $out/mke2fs is up to date with its inputs"
  exit 0
fi

tarball=$build/dl/${E2FSPROGS_URL##*/}
mkdir -p "$build/dl"
if [ ! -f "$tarball" ] || [ "$(sha256sum "$tarball" | cut -d' ' -f1)" != "$E2FSPROGS_SHA256" ]; then
  rm -f "$tarball" "$tarball.part"
  curl --fail --silent --show-error --location --connect-timeout 30 --retry 4 --retry-all-errors \
    --output "$tarball.part" "$E2FSPROGS_URL"
  mv "$tarball.part" "$tarball"
fi
got=$(sha256sum "$tarball" | cut -d' ' -f1)
if [ "$got" != "$E2FSPROGS_SHA256" ]; then
  echo "e2fsprogs: $tarball has SHA-256 $got, not the pinned $E2FSPROGS_SHA256" >&2
  exit 1
fi
echo "e2fsprogs: $tarball has the pinned SHA-256 $E2FSPROGS_SHA256"

src=$build/e2fsprogs-$E2FSPROGS_VERSION
obj=$build/obj
rm -rf "$src" "$obj"
mkdir -p "$obj"
tar -xf "$tarball" -C "$build"

# libuuid and libblkid are the tree's own, so no system copy (a shared object) is found; what
# mke2fs does not link is off.
cd "$obj"
"$src/configure" \
  --disable-nls --disable-fuse2fs --disable-debugfs --disable-imager --disable-resizer \
  --disable-defrag --disable-uuidd --disable-e2initrd-helper --disable-elf-shlibs \
  --disable-bsd-shlibs --disable-rpath --disable-testio-debug \
  --enable-libuuid --enable-libblkid \
  CFLAGS="-O2 -g0 -ffile-prefix-map=$src=. -ffile-prefix-map=$obj=." \
  LDFLAGS="-static -Wl,--build-id=none" >"$build/configure.log" 2>&1 || {
  tail -n 40 "$build/configure.log" >&2
  echo "e2fsprogs: configure failed; the full log is $build/configure.log" >&2
  exit 1
}
{ make -j"$(nproc)" libs && make -C misc -j"$(nproc)" mke2fs; } >"$build/build.log" 2>&1 || {
  tail -n 40 "$build/build.log" >&2
  echo "e2fsprogs: the build failed; the full log is $build/build.log" >&2
  exit 1
}

strip --strip-all -o "$out/mke2fs.part" "$obj/misc/mke2fs"
chmod 0755 "$out/mke2fs.part"
if ! file "$out/mke2fs.part" | grep -Eq 'statically linked|static-pie linked'; then
  echo "e2fsprogs: mke2fs is not a static executable: $(file "$out/mke2fs.part")" >&2
  exit 1
fi
mv "$out/mke2fs.part" "$out/mke2fs"
echo "$stamp" >"$out/e2fsprogs.stamp"
echo "e2fsprogs: built $out/mke2fs, SHA-256 $(sha256sum "$out/mke2fs" | cut -d' ' -f1)"
file "$out/mke2fs"
