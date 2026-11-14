#!/bin/bash
# Builds the naru-orch system extension (ADR 0055): a squashfs of /bin/naru-orchd, /bin/naru-signer
# and /manifest.json, and naru-orch.json, its description, in <output-dir>/extensions. The same
# inputs give the same bytes: the programs are built as the initramfs builds them, the squashfs is
# written by mksquashfs -reproducible with zstd, one processor, root ownership and times fixed by
# SOURCE_DATE_EPOCH (image/build.env).
#
# NARU_VERSION (in the environment, required) is the version the extension is built for: init
# mounts only an image whose manifest names its own version.
#
# Usage: build.sh <build-dir> <output-dir>
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <build-dir> <output-dir>" >&2
  exit 2
fi
: "${NARU_VERSION:?extension: NARU_VERSION is the version the image is built for}"
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
rm -rf "$1"
mkdir -p "$1" "$2/extensions"
build=$(cd "$1" && pwd)
out=$(cd "$2/extensions" && pwd)
# shellcheck source=../../build.env
. "$here/../../build.env"
# shellcheck source=../../common/initramfs/lib.sh
. "$here/../../common/initramfs/lib.sh"

name=naru-orch
tree=$build/tree
mkdir -p "$tree/bin"
gobuild "$root/host" "$tree/bin/naru-signer" ./cmd/naru-signer
gobuild "$root/orch" "$tree/bin/naru-orchd" ./cmd/naru-orchd
require_static "$tree/bin" naru-signer naru-orchd
chmod 0755 "$tree" "$tree/bin" "$tree/bin/naru-signer" "$tree/bin/naru-orchd"
printf '{"name":"%s","version":"%s","services":["naru-orchd","naru-signer"]}\n' "$name" "$NARU_VERSION" >"$tree/manifest.json"
chmod 0644 "$tree/manifest.json"

image=$out/$name.sqfs
rm -f "$image"
mksquashfs "$tree" "$image" -comp zstd -reproducible -all-root -no-xattrs -noappend -processors 1 -quiet >/dev/null
sha=$(sha256sum "$image" | cut -d' ' -f1)
printf '{"name":"%s","version":"%s","sha256":"%s","services":["naru-orchd","naru-signer"]}\n' "$name" "$NARU_VERSION" "$sha" >"$out/$name.json"
echo "extension: $image sha256 $sha for naru $NARU_VERSION"
