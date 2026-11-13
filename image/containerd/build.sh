#!/bin/bash
# Builds containerd, containerd-shim-runc-v2 and runc, statically linked, from the commits
# image/containerd/pin.env names: fetches each commit with git and nowhere else, fails unless HEAD
# is it, and builds with -trimpath.
#
# containerd and its shim are built with CGO_ENABLED=0, and with the tags that keep the btrfs, zfs
# and devmapper snapshotters out (they need C libraries the image does not have). runc links
# libseccomp through cgo, so it is built with the host's C toolchain and statically against a
# libseccomp this script builds from the pinned release tarball (SHA-256 verified) into the build
# directory (with a gperf built the same way, which libseccomp's build runs), the way mke2fs is
# linked (ADR 0011, ADR 0026); netgo and osusergo keep it from
# reading a libc name-service module at run time. A build that cannot link libseccomp fails: no
# runc without seccomp is made. Every result must be a static executable that reports the pinned
# version. A build whose inputs match the last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir>
# <build-dir> keeps the download in dl/ and receives the checkouts and libseccomp;
# <output-dir> receives containerd, containerd-shim-runc-v2 and runc.
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

programs="containerd containerd-shim-runc-v2 runc"

stamp=$(cat "$here/pin.env" "$here/build.sh" "$here/../build.env" | sha256sum | cut -d' ' -f1)
if [ -f "$out/containerd.stamp" ] && [ "$(cat "$out/containerd.stamp")" = "$stamp" ]; then
  complete=1
  for p in $programs; do [ -f "$out/$p" ] || complete=0; done
  if [ "$complete" -eq 1 ]; then
    echo "containerd: the programs in $out are up to date with their inputs"
    exit 0
  fi
fi

checkout() { # <dir> <repo> <commit> <what>
  if [ ! -d "$1/.git" ] || [ "$(git -C "$1" rev-parse HEAD 2>/dev/null)" != "$3" ]; then
    rm -rf "$1"
    git init --quiet "$1"
    git -C "$1" fetch --quiet --depth 1 "$2" "$3"
    git -C "$1" checkout --quiet --detach FETCH_HEAD
  fi
  got=$(git -C "$1" rev-parse HEAD)
  if [ "$got" != "$3" ]; then
    echo "containerd: the $4 checkout is at $got, not the pinned $3" >&2
    exit 1
  fi
}
checkout "$build/containerd" "$CONTAINERD_REPO" "$CONTAINERD_COMMIT" containerd
checkout "$build/runc" "$RUNC_REPO" "$RUNC_COMMIT" runc

go_version=$(awk '$1 == "go" { print $2 }' "$build/containerd/go.mod")
if [ -z "$go_version" ]; then
  echo "containerd: go.mod names no Go release" >&2
  exit 1
fi
export GOTOOLCHAIN=go$go_version
# The checkouts sit under this repository's go.work, which does not list their modules.
export GOWORK=off

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
    echo "containerd: $file has SHA-256 $sum, not the pinned $2" >&2
    exit 1
  fi
  echo "containerd: $file has the pinned SHA-256 $2" >&2
  echo "$file"
}

# gperf, which libseccomp's build runs, into a prefix of its own; then libseccomp, static.
tools=$build/tools
rm -rf "$build/gperf-$GPERF_VERSION" "$build/gperf-obj" "$tools"
mkdir -p "$build/gperf-obj"
tar -xf "$(fetch "$GPERF_URL" "$GPERF_SHA256")" -C "$build"
(cd "$build/gperf-obj" && "$build/gperf-$GPERF_VERSION/configure" --prefix="$tools" >"$build/gperf-build.log" 2>&1 &&
  make -j"$(nproc)" >>"$build/gperf-build.log" 2>&1 && make install >>"$build/gperf-build.log" 2>&1) || {
  tail -n 40 "$build/gperf-build.log" >&2
  echo "containerd: the gperf build failed; the log is $build/gperf-build.log" >&2
  exit 1
}

seccomp_src=$build/libseccomp-$LIBSECCOMP_VERSION
seccomp_obj=$build/libseccomp-obj
seccomp=$build/libseccomp
rm -rf "$seccomp_src" "$seccomp_obj" "$seccomp"
mkdir -p "$seccomp_obj"
tar -xf "$(fetch "$LIBSECCOMP_URL" "$LIBSECCOMP_SHA256")" -C "$build"
(cd "$seccomp_obj" && PATH="$tools/bin:$PATH" "$seccomp_src/configure" --prefix="$seccomp" \
  --enable-static --disable-shared --disable-python --disable-tools \
  CFLAGS="-O2 -g0 -ffile-prefix-map=$seccomp_src=. -ffile-prefix-map=$seccomp_obj=." >"$build/libseccomp-build.log" 2>&1 &&
  PATH="$tools/bin:$PATH" make -j"$(nproc)" >>"$build/libseccomp-build.log" 2>&1 &&
  PATH="$tools/bin:$PATH" make install >>"$build/libseccomp-build.log" 2>&1) || {
  tail -n 40 "$build/libseccomp-build.log" >&2
  echo "containerd: the libseccomp build failed; the log is $build/libseccomp-build.log" >&2
  exit 1
}
if [ ! -f "$seccomp/lib/libseccomp.a" ]; then
  echo "containerd: libseccomp built no static library" >&2
  exit 1
fi

part=$build/bin
rm -rf "$part"
mkdir -p "$part"

pkg=github.com/containerd/containerd/v2
ctr_ldflags="-s -w -buildid= -X $pkg/version.Version=$CONTAINERD_VERSION -X $pkg/version.Revision=$CONTAINERD_COMMIT -X $pkg/version.Package=$pkg"
ctr_tags="urfave_cli_no_docs osusergo netgo static_build no_btrfs no_devmapper no_zfs"
(cd "$build/containerd" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "$ctr_tags" -ldflags="$ctr_ldflags" -o "$part/containerd" ./cmd/containerd)
(cd "$build/containerd" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false \
  -tags "$ctr_tags no_grpc" -ldflags="$ctr_ldflags" -o "$part/containerd-shim-runc-v2" ./cmd/containerd-shim-runc-v2)

# runc: libpathrs, which runc's Makefile adds by default, is a Rust library the image does not
# build; without the tag runc uses its pure Go path-safety code.
(cd "$build/runc" && CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
  PKG_CONFIG_PATH="$seccomp/lib/pkgconfig" PKG_CONFIG="pkg-config --static" \
  CGO_CFLAGS="-O2 -g0 -ffile-prefix-map=$build=." \
  go build -trimpath -buildvcs=false -tags seccomp,urfave_cli_no_docs,netgo,osusergo \
  -ldflags="-s -w -buildid= -linkmode=external -extldflags \"-static -Wl,--build-id=none\" -X main.gitCommit=$RUNC_COMMIT" \
  -o "$part/runc" .)
strip --strip-all "$part/runc"

for p in $programs; do
  if ! file "$part/$p" | grep -Eq 'statically linked|static-pie linked'; then
    echo "containerd: $p is not a static executable: $(file "$part/$p")" >&2
    exit 1
  fi
done
version=$("$part/containerd" --version | awk '{ print $3 }')
if [ "$version" != "$CONTAINERD_VERSION" ]; then
  echo "containerd: containerd reports version $version, not the pinned $CONTAINERD_VERSION" >&2
  exit 1
fi
version=$("$part/containerd-shim-runc-v2" -v | awk '$1 == "Version:" { print $2 }')
if [ "$version" != "$CONTAINERD_VERSION" ]; then
  echo "containerd: containerd-shim-runc-v2 reports version $version, not the pinned $CONTAINERD_VERSION" >&2
  exit 1
fi
runc_version=$("$part/runc" --version)
if [ "$(echo "$runc_version" | awk 'NR == 1 { print $3 }')" != "$RUNC_VERSION" ]; then
  echo "containerd: runc does not report version $RUNC_VERSION: $runc_version" >&2
  exit 1
fi
if [ "$(echo "$runc_version" | awk '$1 == "libseccomp:" { print $2 }')" != "$LIBSECCOMP_VERSION" ]; then
  echo "containerd: runc is not linked against libseccomp $LIBSECCOMP_VERSION: $runc_version" >&2
  exit 1
fi
for p in $programs; do
  cp "$part/$p" "$out/$p"
  echo "containerd: built $out/$p, SHA-256 $(sha256sum "$out/$p" | cut -d' ' -f1)"
done
echo "$stamp" >"$out/containerd.stamp"
