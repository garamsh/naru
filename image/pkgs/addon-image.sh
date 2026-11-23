#!/bin/sh
# The image of a cluster add-on, laid out by hand as an OCI archive, and the check that the archive
# and the manifests that run it name the image alike (ADR 0105, amending ADR 0083 item 3). The archive's
# index names its one image by the reference the manifests use, naru.local/<name>@sha256:<digest of the
# image's manifest>, so the Node's import creates that name and no other, and the reference is written
# once, here, for both. Every byte comes from SOURCE_DATE_EPOCH, so the digest is the same for two builds
# of one commit.
#
# Usage: addon-image.sh build <name> <entrypoint> <rootfs> <archive>   prints the reference
#        addon-image.sh check <name> <archive> <manifest>...
set -eu

sha() { sha256sum "$1" | cut -d' ' -f1; }

build() {
  name=$1 entrypoint=$2 rootfs=$3 archive=$4
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  mkdir -p "$work/oci/blobs/sha256"
  (cd "$rootfs" && find . -mindepth 1 -maxdepth 1 -printf '%P\0' | sort -z |
    tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu --null -T - -cf "$work/layer.tar")
  layer=$(sha "$work/layer.tar")
  printf '{"architecture":"amd64","os":"linux","config":{"Entrypoint":["/%s"]},"rootfs":{"type":"layers","diff_ids":["sha256:%s"]}}' "$entrypoint" "$layer" >"$work/config.json"
  config=$(sha "$work/config.json")
  printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:%s","size":%s},"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar","digest":"sha256:%s","size":%s}]}' \
    "$config" "$(stat -c %s "$work/config.json")" "$layer" "$(stat -c %s "$work/layer.tar")" >"$work/manifest.json"
  digest=$(sha "$work/manifest.json")
  cp "$work/layer.tar" "$work/oci/blobs/sha256/$layer"
  cp "$work/config.json" "$work/oci/blobs/sha256/$config"
  cp "$work/manifest.json" "$work/oci/blobs/sha256/$digest"
  ref="naru.local/$name@sha256:$digest"
  printf '{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:%s","size":%s,"annotations":{"io.containerd.image.name":"%s"}}]}' \
    "$digest" "$(stat -c %s "$work/manifest.json")" "$ref" >"$work/oci/index.json"
  printf '{"imageLayoutVersion":"1.0.0"}' >"$work/oci/oci-layout"
  tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner --format=gnu -cf "$archive" -C "$work/oci" .
  echo "$ref"
}

# check fails unless the archive names one image as naru.local/<name>@sha256:<d>, d is the digest of the
# manifest blob that image's index entry points at, and every naru.local reference in the manifests is
# that name: what containerd imports and what the kubelet looks for are then the same string.
check() {
  name=$1 archive=$2
  shift 2
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  tar -xf "$archive" -C "$work"
  names=$(grep -o '"io.containerd.image.name":"[^"]*"' "$work/index.json" | cut -d'"' -f4)
  [ "$(printf '%s\n' "$names" | wc -l)" -eq 1 ] || { echo "$archive: want one named image, found: $names" >&2; exit 1; }
  digest=${names#naru.local/"$name"@sha256:}
  [ "$digest" != "$names" ] || { echo "$archive: image name $names is not naru.local/$name@sha256:<digest>" >&2; exit 1; }
  [ "$(sha "$work/blobs/sha256/$digest")" = "$digest" ] || { echo "$archive: blob $digest does not hash to its name" >&2; exit 1; }
  grep -q "\"digest\":\"sha256:$digest\"" "$work/index.json" || { echo "$archive: the index does not point at $digest" >&2; exit 1; }
  for m in "$@"; do
    refs=$(grep -o 'naru\.local/[^" ]*' "$m" | sort -u)
    [ "$refs" = "$names" ] || { echo "$m: names '$refs', the archive imports '$names'" >&2; exit 1; }
  done
}

case "${1:-}" in
build) [ "$#" -eq 5 ] || { echo "usage: $0 build <name> <entrypoint> <rootfs> <archive>" >&2; exit 2; }; shift; build "$@" ;;
check) [ "$#" -ge 4 ] || { echo "usage: $0 check <name> <archive> <manifest>..." >&2; exit 2; }; shift; check "$@" ;;
*) echo "usage: $0 build|check ..." >&2; exit 2 ;;
esac
