#!/bin/bash
# Builds kube-apiserver, kube-controller-manager, kube-scheduler, kubelet and kube-proxy, statically
# linked, from the commit image/kubernetes/pin.env names: fetches that commit with git and nowhere
# else, fails unless HEAD is it, builds the five with CGO_ENABLED=0 and -trimpath, stamps the
# version the way Kubernetes' own build does, and fails unless every result is a static executable
# that reports the pinned version. The Go toolchain is the one the checkout's .go-version names,
# which is the release Kubernetes builds that tag with (a go command that is not that version
# downloads it). A build whose inputs match the last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> receives the checkout; <output-dir> receives the five programs.
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

programs="kube-apiserver kube-controller-manager kube-scheduler kubelet kube-proxy"

stamp=$(cat "$here/pin.env" "$here/build.sh" "$here/../build.env" | sha256sum | cut -d' ' -f1)
if [ -f "$out/kubernetes.stamp" ] && [ "$(cat "$out/kubernetes.stamp")" = "$stamp" ]; then
  complete=1
  for p in $programs; do [ -f "$out/$p" ] || complete=0; done
  if [ "$complete" -eq 1 ]; then
    echo "kubernetes: the programs in $out are up to date with their inputs"
    exit 0
  fi
fi

src=$build/kubernetes
if [ ! -d "$src/.git" ] || [ "$(git -C "$src" rev-parse HEAD 2>/dev/null)" != "$KUBERNETES_COMMIT" ]; then
  rm -rf "$src"
  git init --quiet "$src"
  git -C "$src" fetch --quiet --depth 1 "$KUBERNETES_REPO" "$KUBERNETES_COMMIT"
  git -C "$src" checkout --quiet --detach FETCH_HEAD
fi
got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$KUBERNETES_COMMIT" ]; then
  echo "kubernetes: the checkout is at $got, not the pinned $KUBERNETES_COMMIT" >&2
  exit 1
fi

toolchain=go$(tr -d '[:space:]' <"$src/.go-version")
if [ "$toolchain" = go ]; then
  echo "kubernetes: .go-version names no Go release" >&2
  exit 1
fi
export GOTOOLCHAIN=$toolchain
# The checkout sits under this repository's go.work, which does not list Kubernetes' module.
export GOWORK=off

# The values Kubernetes' hack/lib/version.sh stamps: a build from a detached checkout has no tag to
# describe, so they come from the pin.
build_date=$(date -u --date="@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ)
minor=${KUBERNETES_VERSION#v*.}
minor=${minor%%.*}
ldflags="-s -w -buildid="
for pkg in k8s.io/client-go/pkg/version k8s.io/component-base/version; do
  ldflags="$ldflags -X $pkg.gitVersion=$KUBERNETES_VERSION -X $pkg.gitCommit=$KUBERNETES_COMMIT -X $pkg.gitTreeState=clean"
  ldflags="$ldflags -X $pkg.gitMajor=1 -X $pkg.gitMinor=$minor -X $pkg.buildDate=$build_date"
done

part=$build/bin
rm -rf "$part"
mkdir -p "$part"
targets=()
for p in $programs; do targets+=("./cmd/$p"); done
# The tags are Kubernetes' own for a release build; go.sum fixes every dependency.
(cd "$src" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags selinux,notest,grpcnotrace -ldflags="$ldflags" -o "$part" "${targets[@]}")

for p in $programs; do
  if ! file "$part/$p" | grep -Eq 'statically linked|static-pie linked'; then
    echo "kubernetes: $p is not a static executable: $(file "$part/$p")" >&2
    exit 1
  fi
  version=$("$part/$p" --version | awk '{ print $NF }')
  if [ "$version" != "$KUBERNETES_VERSION" ]; then
    echo "kubernetes: $p reports version $version, not the pinned $KUBERNETES_VERSION" >&2
    exit 1
  fi
done
for p in $programs; do
  cp "$part/$p" "$out/$p"
  echo "kubernetes: built $out/$p, SHA-256 $(sha256sum "$out/$p" | cut -d' ' -f1)"
done
echo "$stamp" >"$out/kubernetes.stamp"
