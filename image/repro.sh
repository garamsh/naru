#!/bin/bash
# Compares the SHA256SUMS of two builds of one commit and fails unless every reproducible asset has
# the same digest in both (docs/architecture/image.md §The builder). Two kinds of asset differ
# between any two builds by design and are left out, and the output names each:
#   naru-data-*.qcow2       the data disk holds a TLS authority generated anew for every build;
#   naru-node-*.test.yaml   the test user-data holds a CA and bootstrap tokens generated per build.
# Usage: repro.sh <SHA256SUMS of build A> <SHA256SUMS of build B>
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <SHA256SUMS> <SHA256SUMS>" >&2
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
echo "$a"
echo "image-repro: $(echo "$a" | wc -l) asset(s) have the same SHA-256 in both builds"
echo "image-repro: left out, because every build differs: $(listing "$1" | awk -v skip="$skip" '$1 ~ skip { printf "%s ", $1 }')"
