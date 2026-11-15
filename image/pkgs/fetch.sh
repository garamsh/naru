#!/bin/sh
# Downloads one pinned source and fails unless it has the pinned SHA-256: the only way a package's
# Dockerfile gets a source (docs/architecture/image.md §The package graph).
#
# Usage: fetch.sh <url> <sha256> <file>
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <url> <sha256> <file>" >&2
  exit 2
fi
curl --fail --silent --show-error --location --connect-timeout 30 --retry 4 --retry-all-errors \
  --output "$3" "$1"
got=$(sha256sum "$3" | cut -d' ' -f1)
if [ "$got" != "$2" ]; then
  echo "fetch: $1 has SHA-256 $got, not the pinned $2" >&2
  exit 1
fi
echo "fetch: $3 has the pinned SHA-256 $2"
