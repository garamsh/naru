#!/bin/bash
# Builds aws_signing_helper, statically linked, from the commit image/rolesanywhere/pin.env names:
# fetches that commit with git and nowhere else, fails unless HEAD is it, builds it with -trimpath,
# and fails unless the result is a static executable that reports the pinned version.
#
# The helper links a PKCS#11 library through cgo, so unlike the other Go programs of the image it
# cannot be built with CGO_ENABLED=0 (the build fails with undefined pkcs11 symbols). It is linked
# statically against the host's libc instead, the way mke2fs is (ADR 0011, ADR 0026), with the
# netgo and osusergo tags so that it reads no libc name-service module at run time. The result has
# no run-time dependency on the image. A build whose inputs match the last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> receives the checkout; <output-dir> receives aws_signing_helper.
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
if [ -f "$out/aws_signing_helper" ] && [ -f "$out/rolesanywhere.stamp" ] && [ "$(cat "$out/rolesanywhere.stamp")" = "$stamp" ]; then
  echo "rolesanywhere: $out/aws_signing_helper is up to date with its inputs"
  exit 0
fi

src=$build/rolesanywhere
if [ ! -d "$src/.git" ] || [ "$(git -C "$src" rev-parse HEAD 2>/dev/null)" != "$ROLESANYWHERE_COMMIT" ]; then
  rm -rf "$src"
  git init --quiet "$src"
  git -C "$src" fetch --quiet --depth 1 "$ROLESANYWHERE_REPO" "$ROLESANYWHERE_COMMIT"
  git -C "$src" checkout --quiet --detach FETCH_HEAD
fi
got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$ROLESANYWHERE_COMMIT" ]; then
  echo "rolesanywhere: the checkout is at $got, not the pinned $ROLESANYWHERE_COMMIT" >&2
  exit 1
fi

export GOTOOLCHAIN=$GO_TOOLCHAIN
# The checkout sits under this repository's go.work, which does not list its module.
export GOWORK=off

part=$build/aws_signing_helper.bin
rm -f "$part"
(cd "$src" && CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -tags netgo,osusergo \
  -ldflags="-s -w -buildid= -linkmode=external -extldflags=-static -X github.com/aws/rolesanywhere-credential-helper/cmd.Version=$ROLESANYWHERE_VERSION" \
  -o "$part" main.go)
if ! file "$part" | grep -Eq 'statically linked|static-pie linked'; then
  echo "rolesanywhere: aws_signing_helper is not a static executable: $(file "$part")" >&2
  exit 1
fi
version=$("$part" version | awk 'NR == 1 { print $NF }')
if [ "$version" != "$ROLESANYWHERE_VERSION" ]; then
  echo "rolesanywhere: the binary reports version $version, not the pinned $ROLESANYWHERE_VERSION" >&2
  exit 1
fi
cp "$part" "$out/aws_signing_helper"
echo "$stamp" >"$out/rolesanywhere.stamp"
echo "rolesanywhere: built $out/aws_signing_helper, SHA-256 $(sha256sum "$out/aws_signing_helper" | cut -d' ' -f1)"
