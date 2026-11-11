#!/bin/bash
# Builds the two initrds a Machine needs, the Guest Agent's and the converter's, with mvmd's own
# Makefile in the mvmd checkout image/initramfs/build.sh made, and checks each against the digest
# of pin.env. They are inputs of the tests of Machines and not part of the image (ADR 0006): the
# test puts them on a disk. The Rust toolchain is the one mvmd's rust-toolchain.toml names, which
# rustup installs for the user, and erofs-utils builds itself.
#
# Usage: build.sh <mvmd-checkout> <output-dir>
# <output-dir> receives guest-initrd.cpio, converter-initrd.cpio and guest-inputs.env, which names
# both with their digests. Nothing is built again when both initrds are there and match.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <mvmd-checkout> <output-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
src=$(cd "$1" && pwd)
mkdir -p "$2"
out=$(cd "$2" && pwd)
# shellcheck source=pin.env
. "$here/pin.env"
# shellcheck source=../initramfs/pin.env
. "$here/../initramfs/pin.env"

got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$MVMD_COMMIT" ]; then
  echo "guest: the mvmd checkout is at $got, not the pinned $MVMD_COMMIT" >&2
  exit 1
fi

digest_is() { [ -f "$1" ] && [ "$(sha256sum "$1" | cut -d' ' -f1)" = "$2" ]; }

if ! digest_is "$out/guest-initrd.cpio" "$GUEST_INITRD_SHA256" || ! digest_is "$out/converter-initrd.cpio" "$CONVERTER_INITRD_SHA256"; then
  make -C "$src" guest-initrd converter-initrd
  cp "$src/guest-agent/target/initrd.cpio" "$out/guest-initrd.cpio"
  cp "$src/converter/target/initrd.cpio" "$out/converter-initrd.cpio"
fi
for pair in "guest-initrd.cpio:$GUEST_INITRD_SHA256" "converter-initrd.cpio:$CONVERTER_INITRD_SHA256"; do
  if ! digest_is "$out/${pair%%:*}" "${pair#*:}"; then
    echo "guest: $out/${pair%%:*} does not have the pinned SHA-256 ${pair#*:}" >&2
    exit 1
  fi
  echo "guest: ${pair%%:*} has the pinned SHA-256 ${pair#*:}"
done
cat >"$out/guest-inputs.env" <<ENV
MVMD_INITRD=$out/guest-initrd.cpio
MVMD_INITRD_SHA256=$GUEST_INITRD_SHA256
MVMD_CONVERTER_INITRD=$out/converter-initrd.cpio
MVMD_CONVERTER_INITRD_SHA256=$CONVERTER_INITRD_SHA256
ENV
