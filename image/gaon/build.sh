#!/bin/bash
# Builds the four gaon service binaries, statically linked, from the commit image/gaon/pin.env
# names: fetches that commit with git and nowhere else, fails unless HEAD is it, builds
# node-agent, apiserver, scheduler and controllers with CGO_ENABLED=0 and -trimpath, and fails
# unless each is a static executable. It builds the gaon command line client from the same
# checkout into <output-dir>/gaon for the QEMU tests to run on the host; the initramfs build
# names the four services and does not take it. The Go toolchain is the one gaon's go.work names (a go
# command that is not that version downloads it). A build whose inputs match the last one is
# skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> receives the checkout; <output-dir> receives the binaries.
set -euo pipefail

# A VERSION=<name> given to the outer make (make release) reaches the go command through
# MAKEFLAGS; nothing here takes a variable from the caller.
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

binaries="gaon-node-agent gaon-apiserver gaon-scheduler gaon-controllers"
cli=gaon
stamp=$(cat "$here/pin.env" "$here/build.sh" "$here/../build.env" | sha256sum | cut -d' ' -f1)
up_to_date=1
for b in $binaries $cli; do [ -f "$out/$b" ] || up_to_date=0; done
if [ "$up_to_date" = 1 ] && [ -f "$out/gaon.stamp" ] && [ "$(cat "$out/gaon.stamp")" = "$stamp" ]; then
  echo "gaon: $out is up to date with its inputs"
  exit 0
fi

src=$build/gaon
if [ ! -d "$src/.git" ] || [ "$(git -C "$src" rev-parse HEAD 2>/dev/null)" != "$GAON_COMMIT" ]; then
  rm -rf "$src"
  git init --quiet "$src"
  git -C "$src" fetch --quiet --depth 1 "$GAON_REPO" "$GAON_COMMIT"
  git -C "$src" checkout --quiet --detach FETCH_HEAD
fi
got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$GAON_COMMIT" ]; then
  echo "gaon: the checkout is at $got, not the pinned $GAON_COMMIT" >&2
  exit 1
fi

# The toolchain gaon's workspace names, so the bytes do not depend on the host's go command.
toolchain=$(awk '$1 == "toolchain" { print $2 }' "$src/go.work")
if [ -z "$toolchain" ]; then
  echo "gaon: go.work names no toolchain" >&2
  exit 1
fi
export GOTOOLCHAIN=$toolchain

part=$build/out
rm -rf "$part"
mkdir -p "$part"
for pkg in node-agent/cmd/gaon-node-agent apiserver/cmd/gaon-apiserver scheduler/cmd/gaon-scheduler controllers/cmd/gaon-controllers gaon-cli/cmd/gaon; do
  (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$part/$(basename "$pkg")" "./$pkg")
done
for b in $binaries $cli; do
  if ! file "$part/$b" | grep -Eq 'statically linked|static-pie linked'; then
    echo "gaon: $b is not a static executable: $(file "$part/$b")" >&2
    exit 1
  fi
done
for b in $binaries $cli; do
  cp "$part/$b" "$out/$b"
  echo "gaon: built $out/$b, SHA-256 $(sha256sum "$out/$b" | cut -d' ' -f1)"
done
echo "$stamp" >"$out/gaon.stamp"
