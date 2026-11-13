#!/bin/bash
# Builds docker-credential-ecr-login, statically linked, from the commit image/ecr-login/pin.env
# names: fetches that commit with git and nowhere else, fails unless HEAD is it, builds the
# ecr-login module (it vendors its dependencies) with CGO_ENABLED=0 and -trimpath, and fails unless
# the result is a static executable that reports the pinned version. A build whose inputs match the
# last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> receives the checkout; <output-dir> receives docker-credential-ecr-login.
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
if [ -f "$out/docker-credential-ecr-login" ] && [ -f "$out/ecr-login.stamp" ] && [ "$(cat "$out/ecr-login.stamp")" = "$stamp" ]; then
  echo "ecr-login: $out/docker-credential-ecr-login is up to date with its inputs"
  exit 0
fi

src=$build/ecr-login
if [ ! -d "$src/.git" ] || [ "$(git -C "$src" rev-parse HEAD 2>/dev/null)" != "$ECR_LOGIN_COMMIT" ]; then
  rm -rf "$src"
  git init --quiet "$src"
  git -C "$src" fetch --quiet --depth 1 "$ECR_LOGIN_REPO" "$ECR_LOGIN_COMMIT"
  git -C "$src" checkout --quiet --detach FETCH_HEAD
fi
got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$ECR_LOGIN_COMMIT" ]; then
  echo "ecr-login: the checkout is at $got, not the pinned $ECR_LOGIN_COMMIT" >&2
  exit 1
fi

export GOTOOLCHAIN=$GO_TOOLCHAIN
# The checkout sits under this repository's go.work, which does not list its module.
export GOWORK=off

part=$build/docker-credential-ecr-login.bin
rm -f "$part"
pkg=github.com/awslabs/amazon-ecr-credential-helper/ecr-login
(cd "$src/ecr-login" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-mod=vendor go build -trimpath -buildvcs=false \
  -ldflags="-s -w -buildid= -X $pkg/version.Version=$ECR_LOGIN_VERSION -X $pkg/version.GitCommitSHA=$ECR_LOGIN_COMMIT" \
  -o "$part" ./cli/docker-credential-ecr-login)
if ! file "$part" | grep -Eq 'statically linked|static-pie linked'; then
  echo "ecr-login: docker-credential-ecr-login is not a static executable: $(file "$part")" >&2
  exit 1
fi
version=$("$part" version | awk 'NR == 1 { print $NF }')
if [ "$version" != "$ECR_LOGIN_VERSION" ]; then
  echo "ecr-login: the binary reports version $version, not the pinned $ECR_LOGIN_VERSION" >&2
  exit 1
fi
cp "$part" "$out/docker-credential-ecr-login"
echo "$stamp" >"$out/ecr-login.stamp"
echo "ecr-login: built $out/docker-credential-ecr-login, SHA-256 $(sha256sum "$out/docker-credential-ecr-login" | cut -d' ' -f1)"
