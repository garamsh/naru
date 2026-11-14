#!/bin/bash
# Builds the CNI plugins bridge, host-local and loopback, statically linked, from the commit
# image/node/cni/pin.env names: fetches that commit with git and nowhere else, fails unless HEAD is it,
# builds them with CGO_ENABLED=0 and -trimpath, and fails unless every result is a static
# executable that reports the pinned version. The Go toolchain is the one the module's go.mod
# names (a go command that is not that version downloads it). A build whose inputs match the last
# one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> receives the checkout; <output-dir> receives the three plugins.
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

# plugin name and the directory its package is in
plugins="bridge:plugins/main/bridge host-local:plugins/ipam/host-local loopback:plugins/main/loopback"

stamp=$(cat "$here/pin.env" "$here/build.sh" "$here/../../build.env" | sha256sum | cut -d' ' -f1)
if [ -f "$out/cni.stamp" ] && [ "$(cat "$out/cni.stamp")" = "$stamp" ]; then
  complete=1
  for p in $plugins; do [ -f "$out/${p%%:*}" ] || complete=0; done
  if [ "$complete" -eq 1 ]; then
    echo "cni: the plugins in $out are up to date with their inputs"
    exit 0
  fi
fi

src=$build/cni
if [ ! -d "$src/.git" ] || [ "$(git -C "$src" rev-parse HEAD 2>/dev/null)" != "$CNI_COMMIT" ]; then
  rm -rf "$src"
  git init --quiet "$src"
  git -C "$src" fetch --quiet --depth 1 "$CNI_REPO" "$CNI_COMMIT"
  git -C "$src" checkout --quiet --detach FETCH_HEAD
fi
got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$CNI_COMMIT" ]; then
  echo "cni: the checkout is at $got, not the pinned $CNI_COMMIT" >&2
  exit 1
fi

go_version=$(awk '$1 == "go" { print $2 }' "$src/go.mod")
if [ -z "$go_version" ]; then
  echo "cni: go.mod names no Go release" >&2
  exit 1
fi
export GOTOOLCHAIN=go$go_version
# The checkout sits under this repository's go.work, which does not list its module.
export GOWORK=off

part=$build/bin
rm -rf "$part"
mkdir -p "$part"
for p in $plugins; do
  (cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
    -ldflags="-s -w -buildid= -X github.com/containernetworking/plugins/pkg/utils/buildversion.BuildVersion=$CNI_VERSION" \
    -o "$part/${p%%:*}" "./${p#*:}")
  name=${p%%:*}
  if ! file "$part/$name" | grep -Eq 'statically linked|static-pie linked'; then
    echo "cni: $name is not a static executable: $(file "$part/$name")" >&2
    exit 1
  fi
  # A plugin run with no CNI_COMMAND names itself and its version in its error.
  version=$(env -u CNI_COMMAND "$part/$name" 2>&1 | grep -o "CNI $name plugin .*" | awk '{ print $4 }' || true)
  if [ "$version" != "$CNI_VERSION" ]; then
    echo "cni: $name reports version '$version', not the pinned $CNI_VERSION" >&2
    exit 1
  fi
done
for p in $plugins; do
  cp "$part/${p%%:*}" "$out/${p%%:*}"
  echo "cni: built $out/${p%%:*}, SHA-256 $(sha256sum "$out/${p%%:*}" | cut -d' ' -f1)"
done
echo "$stamp" >"$out/cni.stamp"
