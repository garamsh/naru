#!/bin/bash
# Builds mkfs.xfs, statically linked, from the pinned xfsprogs tarball and the pinned sources of the
# libraries it links (ADR 0027): fetches each, verifies its SHA-256, builds libuuid and libblkid
# (util-linux), liburcu and libinih into a private prefix as static archives, then configures an
# xfsprogs build against that prefix, builds mkfs.xfs only, strips it and fails unless it is a
# static executable. Sources are unpacked afresh before each build; the build paths are mapped away
# and nothing is stamped, so the same inputs give the same bytes (image/build.env fixes the
# environment). A build whose inputs match the last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> keeps the downloads in dl/ and receives the sources and object directories;
# <output-dir> receives mkfs.xfs.
set -euo pipefail

# A VERSION=<name> given to the outer make (make release) reaches these makes through MAKEFLAGS; the
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
# shellcheck source=../build.env
. "$here/../build.env"

stamp=$(cat "$here/pin.env" "$here/build.sh" "$here/../build.env" | sha256sum | cut -d' ' -f1)
if [ -f "$out/mkfs.xfs" ] && [ -f "$out/xfsprogs.stamp" ] && [ "$(cat "$out/xfsprogs.stamp")" = "$stamp" ]; then
  echo "xfsprogs: $out/mkfs.xfs is up to date with its inputs"
  exit 0
fi

mkdir -p "$build/dl"
# fetch <name> <url> <sha256> prints the path of the verified download.
fetch() {
  local f=$build/dl/$1-${2##*/}
  if [ ! -f "$f" ] || [ "$(sha256sum "$f" | cut -d' ' -f1)" != "$3" ]; then
    rm -f "$f" "$f.part"
    curl --fail --silent --show-error --location --connect-timeout 30 --retry 4 --retry-all-errors \
      --output "$f.part" "$2"
    mv "$f.part" "$f"
  fi
  local got
  got=$(sha256sum "$f" | cut -d' ' -f1)
  if [ "$got" != "$3" ]; then
    echo "xfsprogs: $f has SHA-256 $got, not the pinned $3" >&2
    exit 1
  fi
  echo "xfsprogs: $f has the pinned SHA-256 $3" >&2
  echo "$f"
}
xfs_tar=$(fetch xfsprogs "$XFSPROGS_URL" "$XFSPROGS_SHA256")
ul_tar=$(fetch util-linux "$UTIL_LINUX_URL" "$UTIL_LINUX_SHA256")
urcu_tar=$(fetch urcu "$URCU_URL" "$URCU_SHA256")
inih_tar=$(fetch inih "$INIH_URL" "$INIH_SHA256")

work=$build/work
deps=$work/deps
rm -rf "$work"
mkdir -p "$work" "$deps/include" "$deps/lib"
tar -xf "$xfs_tar" -C "$work"
tar -xf "$ul_tar" -C "$work"
tar -xf "$urcu_tar" -C "$work"
tar -xf "$inih_tar" -C "$work"

log=$work/build.log
: >"$log"
fail() {
  tail -n 40 "$log" >&2
  echo "xfsprogs: $1 failed; the full log is $log" >&2
  exit 1
}
cflags="-O2 -g0 -ffile-prefix-map=$work=."

# util-linux: the two libraries only, static.
mkdir -p "$work/obj-ul"
(cd "$work/obj-ul" && "$work/util-linux-$UTIL_LINUX_VERSION/configure" --prefix="$deps" \
  --disable-all-programs --enable-libuuid --enable-libblkid --disable-nls --disable-shared \
  --enable-static --without-python --localstatedir=/var --runstatedir=/run CFLAGS="$cflags" \
  && make -j"$(nproc)" && make install) >>"$log" 2>&1 || fail "util-linux"

# userspace-rcu, static.
mkdir -p "$work/obj-urcu"
(cd "$work/obj-urcu" && "$work/userspace-rcu-$URCU_VERSION/configure" --prefix="$deps" \
  --disable-shared --enable-static CFLAGS="$cflags" \
  && make -j"$(nproc)" && make install) >>"$log" 2>&1 || fail "userspace-rcu"

# inih: one source file and one header, archived by hand.
(cd "$work/inih-$INIH_VERSION" \
  && gcc $cflags -c ini.c -o "$work/ini.o" \
  && ar rcsD "$deps/lib/libinih.a" "$work/ini.o" \
  && cp ini.h "$deps/include/ini.h") >>"$log" 2>&1 || fail "inih"

# xfsprogs: mkfs.xfs only, against that prefix. Its build runs in the unpacked tree.
cd "$work/xfsprogs-$XFSPROGS_VERSION"
PKG_CONFIG_LIBDIR="$deps/lib/pkgconfig" ./configure --disable-shared --enable-scrub=no \
  --enable-editline=no --enable-lib64=no --enable-gettext=no \
  CFLAGS="$cflags -I$deps/include" CXXFLAGS="$cflags -I$deps/include" \
  LDFLAGS="-static -Wl,--build-id=none -L$deps/lib" >>"$log" 2>&1 || fail "xfsprogs configure"
# The tree's makefile discards the LDFLAGS of the environment and of configure, so the link flags
# are given to make (libtool takes -all-static for a static executable); the header directory is set up first.
make headers >>"$log" 2>&1 || fail "xfsprogs headers"
make -j"$(nproc)" LDFLAGS="-all-static -Wl,--build-id=none -L$deps/lib" mkfs >>"$log" 2>&1 || fail "xfsprogs build"

strip --strip-all -o "$out/mkfs.xfs.part" "$work/xfsprogs-$XFSPROGS_VERSION/mkfs/mkfs.xfs"
chmod 0755 "$out/mkfs.xfs.part"
if ! file "$out/mkfs.xfs.part" | grep -Eq 'statically linked|static-pie linked'; then
  echo "xfsprogs: mkfs.xfs is not a static executable: $(file "$out/mkfs.xfs.part")" >&2
  exit 1
fi
mv "$out/mkfs.xfs.part" "$out/mkfs.xfs"
echo "$stamp" >"$out/xfsprogs.stamp"
echo "xfsprogs: built $out/mkfs.xfs, SHA-256 $(sha256sum "$out/mkfs.xfs" | cut -d' ' -f1)"
file "$out/mkfs.xfs"
