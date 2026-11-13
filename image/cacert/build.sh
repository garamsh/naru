#!/bin/bash
# Writes ca-certificates.crt: the pinned public root bundle (image/cacert/pin.env), downloaded with
# curl and checked against its SHA-256. With a third argument, the PEM file of a certificate is
# appended after the bundle: the test image's throwaway root (ADR 0026), which the release image
# never carries. A build whose inputs match the last one is skipped.
#
# Usage: build.sh <build-dir> <output-dir> [<extra-root.pem>]
# <build-dir> receives the download; <output-dir> receives ca-certificates.crt.
set -euo pipefail

unset MAKEFLAGS MFLAGS

if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
  echo "usage: $0 <build-dir> <output-dir> [<extra-root.pem>]" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$1" "$2"
build=$(cd "$1" && pwd)
out=$(cd "$2" && pwd)
extra=${3:-}
# shellcheck source=pin.env
. "$here/pin.env"

extra_sum=none
if [ -n "$extra" ]; then
  extra_sum=$(sha256sum "$extra" | cut -d' ' -f1)
fi
stamp=$(cat "$here/pin.env" "$here/build.sh" | sha256sum | cut -d' ' -f1)-$extra_sum
if [ -f "$out/ca-certificates.crt" ] && [ -f "$out/cacert.stamp" ] && [ "$(cat "$out/cacert.stamp")" = "$stamp" ]; then
  echo "cacert: $out/ca-certificates.crt is up to date with its inputs"
  exit 0
fi

dl=$build/${CACERT_URL##*/}
if [ ! -f "$dl" ] || [ "$(sha256sum "$dl" | cut -d' ' -f1)" != "$CACERT_SHA256" ]; then
  rm -f "$dl"
  curl --fail --silent --show-error --location --max-time 120 --output "$dl.part" "$CACERT_URL"
  mv "$dl.part" "$dl"
fi
got=$(sha256sum "$dl" | cut -d' ' -f1)
if [ "$got" != "$CACERT_SHA256" ]; then
  echo "cacert: $CACERT_URL has SHA-256 $got, not the pinned $CACERT_SHA256" >&2
  exit 1
fi
if [ "$(grep -c '^-----BEGIN CERTIFICATE-----$' "$dl")" -eq 0 ]; then
  echo "cacert: $dl holds no certificate" >&2
  exit 1
fi
part=$build/ca-certificates.crt.part
cp "$dl" "$part"
if [ -n "$extra" ]; then
  cat "$extra" >>"$part"
fi
mv "$part" "$out/ca-certificates.crt"
echo "$stamp" >"$out/cacert.stamp"
echo "cacert: wrote $out/ca-certificates.crt from $CACERT_URL, SHA-256 $(sha256sum "$out/ca-certificates.crt" | cut -d' ' -f1)${extra:+, with $extra appended}"
