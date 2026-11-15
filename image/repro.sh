#!/bin/bash
# Compares the SHA256SUMS of two builds of one commit and fails unless every reproducible asset has
# the same digest in both (docs/architecture/image.md §The builder). Two kinds of asset differ
# between any two builds by design and are left out, and the output names each:
#   naru-data-*.qcow2       the data disk holds a TLS authority generated anew for every build;
#   naru-node-*.test.yaml   the test user-data holds a CA and bootstrap tokens generated per build.
# With the package directories of the two builds (image/pkgs/build.sh's output, one OCI tar per
# package) it also fails unless every package has the same manifest digest in both; a reference
# SHA256SUMS comes from a machine whose packages are not at hand, so they are then not compared.
# Usage: repro.sh <SHA256SUMS of build A> <SHA256SUMS of build B> [<packages of build A> <packages of build B>]
set -euo pipefail

if [ "$#" -ne 2 ] && [ "$#" -ne 4 ]; then
  echo "usage: $0 <SHA256SUMS> <SHA256SUMS> [<packages-dir> <packages-dir>]" >&2
  exit 2
fi
for f in "$1" "$2"; do
  if [ ! -s "$f" ]; then
    echo "image-repro: $f is missing or empty" >&2
    exit 1
  fi
done

skip='^(naru-data-.*\.qcow2|naru-node-[0-9]+\.test\.yaml)$'
# name, digest: the sums sorted by name, with and without the assets that differ by design.
listing() { awk '{ print $2, $1 }' "$1" | sort; }
a=$(listing "$1" | awk -v skip="$skip" '$1 !~ skip')
b=$(listing "$2" | awk -v skip="$skip" '$1 !~ skip')
if [ -z "$a" ] || [ "$a" != "$b" ]; then
  echo "image-repro: the reproducible assets differ between $1 and $2:" >&2
  diff <(echo "$a") <(echo "$b") >&2 || true
  exit 1
fi
if [ "$#" -eq 4 ]; then
  root=$(cd "$(dirname "$0")/.." && pwd)
  # name, manifest digest of every package, from naru-pkg of tooling/disk.
  pkgs() { (cd "$root/tooling/disk" && go run ./cmd/naru-pkg digest "$1"/*.tar); }
  pa=$(pkgs "$(cd "$3" && pwd)")
  pb=$(pkgs "$(cd "$4" && pwd)")
  if [ -z "$pa" ] || [ "$pa" != "$pb" ]; then
    echo "image-repro: the packages differ between $3 and $4:" >&2
    diff <(echo "$pa") <(echo "$pb") >&2 || true
    exit 1
  fi
  echo "$pa"
  echo "image-repro: $(echo "$pa" | wc -l) package(s) have the same manifest digest in both builds"
else
  echo "image-repro: the packages are not compared: no package directories were given"
fi
echo "$a"
echo "image-repro: $(echo "$a" | wc -l) asset(s) have the same SHA-256 in both builds"
echo "image-repro: left out, because every build differs: $(listing "$1" | awk -v skip="$skip" '$1 ~ skip { printf "%s ", $1 }')"
