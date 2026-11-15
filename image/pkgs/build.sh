#!/bin/bash
# Builds every package of image/pkgs/docker-bake.hcl in the pinned BuildKit of image/builder/builder.env
# and writes each as an OCI tar, <out-dir>/<name>.tar (docs/architecture/image.md §The package graph,
# ADR 0075). It runs on the host, because BuildKit needs the Docker daemon, which the builder
# container cannot reach. It fails unless the stage-0 builder the packages start from has the config
# digest of builder.env, the same one image/builder/run.sh checks. NARU_PKGS_NO_CACHE=1 builds
# every stage anew, which make image-repro sets so that its two builds share nothing.
#
# Usage: build.sh <out-dir>
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <out-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$1"
out=$(cd "$1" && pwd)
# shellcheck source=../builder/builder.env
. "$here/../builder/builder.env"
# shellcheck source=../build.env
. "$here/../build.env"

docker=${DOCKER:-docker}
if ! "$docker" version >/dev/null 2>&1; then
  echo "pkgs: no container engine is reachable by this user: $docker version fails" >&2
  exit 1
fi
# The BuildKit container run.sh creates; see there for why it is not the daemon's own BuildKit.
bk=naru-buildkit-${BUILDKIT_IMAGE##*:}
bk=${bk:0:30}
if ! "$docker" buildx inspect "$bk" >/dev/null 2>&1; then
  "$docker" buildx create --name "$bk" --driver docker-container --driver-opt "image=$BUILDKIT_IMAGE" >&2
fi

rm -f "$out"/*.tar "$out/metadata.json"
export OUT=$out
cd "$here"
"$docker" buildx bake --builder "$bk" --file docker-bake.hcl --provenance=false --sbom=false \
  --allow fs.read=../builder --allow fs.read=../common/kernel --allow "fs.write=$out" \
  ${NARU_PKGS_NO_CACHE:+--no-cache} --metadata-file "$out/metadata.json" >&2

got=$(awk '/^  "builder": \{/ { in_b = 1 } in_b && /"containerimage.config.digest"/ { gsub(/[",]/, "", $2); print $2; exit }' "$out/metadata.json")
if [ "$got" != "$BUILDER_CONFIG_DIGEST" ]; then
  echo "pkgs: the builder the packages start from has config digest ${got:-none}, not the $BUILDER_CONFIG_DIGEST of builder.env" >&2
  exit 1
fi
rm -f "$out/builder.tar" "$out/metadata.json"
set -- "$out"/*.tar
echo "pkgs: built $# package(s) into $out" >&2
