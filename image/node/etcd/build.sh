#!/bin/bash
# Builds the etcd server, statically linked, from the commit image/node/etcd/pin.env names: fetches
# that commit with git and nowhere else, fails unless HEAD is it, builds the server module with
# CGO_ENABLED=0 and -trimpath, and fails unless the result is a static executable that reports the
# pinned version. The Go toolchain is the one the server module's go.mod names (a go command that
# is not that version downloads it). A build whose inputs match the last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> receives the checkout; <output-dir> receives etcd.
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
if [ -f "$out/etcd" ] && [ -f "$out/etcd.stamp" ] && [ "$(cat "$out/etcd.stamp")" = "$stamp" ]; then
  echo "etcd: $out/etcd is up to date with its inputs"
  exit 0
fi

src=$build/etcd
if [ ! -d "$src/.git" ] || [ "$(git -C "$src" rev-parse HEAD 2>/dev/null)" != "$ETCD_COMMIT" ]; then
  rm -rf "$src"
  git init --quiet "$src"
  git -C "$src" fetch --quiet --depth 1 "$ETCD_REPO" "$ETCD_COMMIT"
  git -C "$src" checkout --quiet --detach FETCH_HEAD
fi
got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$ETCD_COMMIT" ]; then
  echo "etcd: the checkout is at $got, not the pinned $ETCD_COMMIT" >&2
  exit 1
fi

toolchain=$(awk '$1 == "toolchain" { print $2 }' "$src/server/go.mod")
if [ -z "$toolchain" ]; then
  echo "etcd: server/go.mod names no toolchain" >&2
  exit 1
fi
export GOTOOLCHAIN=$toolchain
# The checkout sits under this repository's go.work, which does not list etcd's module.
export GOWORK=off

part=$build/etcd.bin
rm -f "$part"
(cd "$src/server" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$part" .)
if ! file "$part" | grep -Eq 'statically linked|static-pie linked'; then
  echo "etcd: etcd is not a static executable: $(file "$part")" >&2
  exit 1
fi
version=$("$part" --version | awk '/^etcd Version:/ { print $3 }')
if [ "$version" != "$ETCD_VERSION" ]; then
  echo "etcd: the binary reports version $version, not the pinned $ETCD_VERSION" >&2
  exit 1
fi
cp "$part" "$out/etcd"
echo "$stamp" >"$out/etcd.stamp"
echo "etcd: built $out/etcd, SHA-256 $(sha256sum "$out/etcd" | cut -d' ' -f1)"
