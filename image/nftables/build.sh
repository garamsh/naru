#!/bin/bash
# Builds nft, statically linked, from the pinned jansson, libmnl, libnftnl and nftables tarballs (ADR 0011):
# fetches each, verifies its SHA-256, builds the three libraries static into a prefix of their own,
# configures nftables out of tree with JSON output and without the interactive shell, xtables support and
# the documentation (kube-proxy runs nft with a script on stdin and needs none of them), builds nft
# only, strips it and fails unless it is a static executable that reports the pinned version. The
# source is unpacked and the object directories emptied before each build; the build paths are
# mapped away, nft's configuration directory is /etc rather than a path under the prefix (which would
# be compiled into nft) and nothing is stamped, so the same inputs give the same bytes (image/build.env fixes
# the environment). A build whose inputs match the last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> keeps the downloads in dl/ and receives the sources, objects and the library prefix;
# <output-dir> receives nft.
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
# shellcheck source=../build.env
. "$here/../build.env"

stamp=$(cat "$here/pin.env" "$here/build.sh" "$here/../build.env" | sha256sum | cut -d' ' -f1)
if [ -f "$out/nft" ] && [ -f "$out/nftables.stamp" ] && [ "$(cat "$out/nftables.stamp")" = "$stamp" ]; then
  echo "nftables: $out/nft is up to date with its inputs"
  exit 0
fi

fetch() { # <url> <sha256>: downloads into dl/ and prints the verified path
  local file=$build/dl/${1##*/} sum
  mkdir -p "$build/dl"
  if [ ! -f "$file" ] || [ "$(sha256sum "$file" | cut -d' ' -f1)" != "$2" ]; then
    rm -f "$file" "$file.part"
    curl --fail --silent --show-error --location --connect-timeout 30 --retry 4 --retry-all-errors \
      --output "$file.part" "$1"
    mv "$file.part" "$file"
  fi
  sum=$(sha256sum "$file" | cut -d' ' -f1)
  if [ "$sum" != "$2" ]; then
    echo "nftables: $file has SHA-256 $sum, not the pinned $2" >&2
    exit 1
  fi
  echo "nftables: $file has the pinned SHA-256 $2" >&2
  echo "$file"
}

prefix=$build/prefix
rm -rf "$prefix"
mkdir -p "$prefix"

# configure_build <name> <source-dir> <configure arguments>: out of tree, into the prefix, which the
# build reads headers and libraries from (nftables's configure looks for jansson there). libtool
# links a program against shared libraries unless make gives it -all-static. The install puts nft's
# configuration files under the prefix, not into /etc, which nft is configured to read.
configure_build() {
  local name=$1 src=$2 obj=$build/$1-obj
  shift 2
  rm -rf "$obj"
  mkdir -p "$obj"
  (cd "$obj" && "$src/configure" --prefix="$prefix" --disable-shared --enable-static "$@" \
    CFLAGS="-O2 -g0 -ffile-prefix-map=$src=. -ffile-prefix-map=$obj=." \
    CPPFLAGS="-I$prefix/include" LDFLAGS="-static -L$prefix/lib -Wl,--build-id=none" >"$build/$name-build.log" 2>&1 &&
    make -j"$(nproc)" LDFLAGS="-all-static -Wl,--build-id=none" >>"$build/$name-build.log" 2>&1 &&
    make install sysconfdir="$prefix/etc" >>"$build/$name-build.log" 2>&1) || {
    tail -n 40 "$build/$name-build.log" >&2
    echo "nftables: the $name build failed; the full log is $build/$name-build.log" >&2
    exit 1
  }
}

rm -rf "$build/libmnl-$LIBMNL_VERSION" "$build/libnftnl-$LIBNFTNL_VERSION" "$build/nftables-$NFTABLES_VERSION" "$build/jansson-$JANSSON_VERSION"
tar -xf "$(fetch "$JANSSON_URL" "$JANSSON_SHA256")" -C "$build"
tar -xf "$(fetch "$LIBMNL_URL" "$LIBMNL_SHA256")" -C "$build"
tar -xf "$(fetch "$LIBNFTNL_URL" "$LIBNFTNL_SHA256")" -C "$build"
tar -xf "$(fetch "$NFTABLES_URL" "$NFTABLES_SHA256")" -C "$build"
export PKG_CONFIG_PATH=$prefix/lib/pkgconfig
configure_build jansson "$build/jansson-$JANSSON_VERSION"
configure_build libmnl "$build/libmnl-$LIBMNL_VERSION"
configure_build libnftnl "$build/libnftnl-$LIBNFTNL_VERSION"
configure_build nftables "$build/nftables-$NFTABLES_VERSION" \
  --without-cli --disable-python --disable-man-doc --with-mini-gmp --disable-debug --with-json \
  --without-xtables --sysconfdir=/etc

part=$out/nft.part
strip --strip-all -o "$part" "$prefix/sbin/nft"
chmod 0755 "$part"
if ! file "$part" | grep -Eq 'statically linked|static-pie linked'; then
  echo "nftables: nft is not a static executable: $(file "$part")" >&2
  exit 1
fi
version=$("$part" --version | awk '{ print $2 }')
if [ "$version" != "v$NFTABLES_VERSION" ]; then
  echo "nftables: nft reports version $version, not the pinned v$NFTABLES_VERSION" >&2
  exit 1
fi
mv "$part" "$out/nft"
echo "$stamp" >"$out/nftables.stamp"
echo "nftables: built $out/nft, SHA-256 $(sha256sum "$out/nft" | cut -d' ' -f1)"
file "$out/nft"
