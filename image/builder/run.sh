#!/bin/bash
# Runs a command in the builder container (docs/architecture/image.md §The builder, ADR 0063): builds
# the image from image/builder/Dockerfile unless this machine already holds it, fails unless the
# image has the config digest of builder.env, then runs the command with the repository mounted at
# /naru as the calling user. Any Docker daemon the user can reach works, rootful or not; DOCKER=podman
# names another engine with the same command line (buildx, load, tag, run).
#
# Usage: run.sh <command> [<argument>...]
# The command runs in /naru, the repository root. The caches (Go modules, Rust toolchains) are in
# build/cache of the repository, so they survive a run and nothing is written elsewhere.
set -euo pipefail

if [ "$#" -lt 1 ]; then
  echo "usage: $0 <command> [<argument>...]" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
# shellcheck source=builder.env
. "$here/builder.env"
# shellcheck source=../build.env
. "$here/../build.env"

docker=${DOCKER:-docker}
if ! "$docker" version >/dev/null 2>&1; then
  echo "builder: no container engine is reachable by this user: $docker version fails" >&2
  exit 1
fi

name=${BUILDER_CONFIG_DIGEST#sha256:}
tag=naru-builder:$name
if ! "$docker" image inspect "$tag" >/dev/null 2>&1; then
  out=$root/build/builder
  rm -rf "$out"
  mkdir -p "$out"
  echo "builder: building $tag from $here/Dockerfile" >&2
  # --builder default is the Docker daemon's own BuildKit. rewrite-timestamp, the fixed epoch and no
  # attestations give the same layers on every machine.
  "$docker" buildx build --builder default --provenance=false --sbom=false \
    --build-arg SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" \
    --output "type=oci,dest=$out/builder.tar,rewrite-timestamp=true" \
    --metadata-file "$out/metadata.json" "$here" >&2
  got=$(sed -n 's/.*"containerimage.config.digest": "\(sha256:[0-9a-f]*\)".*/\1/p' "$out/metadata.json")
  if [ "$got" != "$BUILDER_CONFIG_DIGEST" ]; then
    echo "builder: the Dockerfile builds an image with config digest ${got:-none}, not the $BUILDER_CONFIG_DIGEST of builder.env" >&2
    exit 1
  fi
  loaded=$("$docker" load -i "$out/builder.tar" | sed -n 's/^Loaded image ID: //p')
  "$docker" tag "$loaded" "$tag"
  rm -rf "$out"
fi
echo "builder: $tag" >&2

mkdir -p "$root/build/cache/home"
exec "$docker" run --rm --init \
  --user "$(id -u):$(id -g)" \
  --env HOME=/naru/build/cache/home \
  --env NARU_BUILDER="$name" \
  --volume "$root:/naru" \
  --workdir /naru \
  "$tag" "$@"
