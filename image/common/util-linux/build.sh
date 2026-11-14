#!/bin/bash
# Builds mount and umount, statically linked, from the pinned util-linux tarball (ADR 0011):
# fetches it, verifies its SHA-256, configures an out-of-tree build with every program off but the
# two and the libraries they link, builds them, strips them and fails unless both are static
# executables that report the pinned version. The source is unpacked and the object directory
# emptied before each build; the build paths are mapped away and nothing is stamped, so the same
# inputs give the same bytes (image/build.env fixes the environment). A build whose inputs match
# the last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> keeps the download in dl/ and receives the source and the object directory;
# <output-dir> receives mount and umount.
set -euo pipefail

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
if [ -f "$out/mount" ] && [ -f "$out/umount" ] && [ -f "$out/util-linux.stamp" ] && [ "$(cat "$out/util-linux.stamp")" = "$stamp" ]; then
  echo "util-linux: $out/mount and $out/umount are up to date with their inputs"
  exit 0
fi

tarball=$build/dl/${UTIL_LINUX_URL##*/}
mkdir -p "$build/dl"
if [ ! -f "$tarball" ] || [ "$(sha256sum "$tarball" | cut -d' ' -f1)" != "$UTIL_LINUX_SHA256" ]; then
  rm -f "$tarball" "$tarball.part"
  curl --fail --silent --show-error --location --connect-timeout 30 --retry 4 --retry-all-errors \
    --output "$tarball.part" "$UTIL_LINUX_URL"
  mv "$tarball.part" "$tarball"
fi
got=$(sha256sum "$tarball" | cut -d' ' -f1)
if [ "$got" != "$UTIL_LINUX_SHA256" ]; then
  echo "util-linux: $tarball has SHA-256 $got, not the pinned $UTIL_LINUX_SHA256" >&2
  exit 1
fi
echo "util-linux: $tarball has the pinned SHA-256 $UTIL_LINUX_SHA256"

src=$build/util-linux-$UTIL_LINUX_VERSION
obj=$build/obj
rm -rf "$src" "$obj"
mkdir -p "$obj"
tar -xf "$tarball" -C "$build"

# libmount, libblkid and libuuid are the tree's own, so no system copy (a shared object) is found.
cd "$obj"
"$src/configure" \
  --disable-all-programs --enable-mount --enable-libmount --enable-libblkid --enable-libuuid \
  --enable-static-programs=mount,umount --disable-shared --enable-static \
  --disable-nls --disable-rpath --disable-makeinstall-chown --disable-bash-completion \
  --without-udev --without-systemd --without-selinux --without-ncurses --without-python \
  --without-tinfo --without-readline --without-libz --without-user --without-util \
  CFLAGS="-O2 -g0 -ffile-prefix-map=$src=. -ffile-prefix-map=$obj=." \
  LDFLAGS="-Wl,--build-id=none" >"$build/configure.log" 2>&1 || {
  tail -n 40 "$build/configure.log" >&2
  echo "util-linux: configure failed; the full log is $build/configure.log" >&2
  exit 1
}
make -j"$(nproc)" mount.static umount.static >"$build/build.log" 2>&1 || {
  tail -n 40 "$build/build.log" >&2
  echo "util-linux: the build failed; the full log is $build/build.log" >&2
  exit 1
}

for p in mount umount; do
  strip --strip-all -o "$out/$p.part" "$obj/$p.static"
  chmod 0755 "$out/$p.part"
  if ! file "$out/$p.part" | grep -Eq 'statically linked|static-pie linked'; then
    echo "util-linux: $p is not a static executable: $(file "$out/$p.part")" >&2
    exit 1
  fi
  version=$("$out/$p.part" --version | awk 'NR == 1 { print $4 }')
  if [ "$version" != "$UTIL_LINUX_VERSION" ]; then
    echo "util-linux: $p reports version $version, not the pinned $UTIL_LINUX_VERSION" >&2
    exit 1
  fi
done
for p in mount umount; do
  mv "$out/$p.part" "$out/$p"
  echo "util-linux: built $out/$p, SHA-256 $(sha256sum "$out/$p" | cut -d' ' -f1)"
done
echo "$stamp" >"$out/util-linux.stamp"
