#!/bin/bash
# Puts mvmd's source at the commit image/initramfs/pin.env names into <dir>, fetched from
# MVMD_REPO at that commit and nowhere else, and fails unless HEAD is that commit. A checkout that
# is already there at the commit is kept. image/initramfs/build.sh and image/guest/build.sh both
# build from it, so the programs and the guest initrds come from one commit.
#
# Usage: mvmd-checkout.sh <dir>
set -euo pipefail

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=initramfs/pin.env
. "$here/initramfs/pin.env"

src=$1
if [ ! -d "$src/.git" ] || [ "$(git -C "$src" rev-parse HEAD 2>/dev/null)" != "$MVMD_COMMIT" ]; then
  rm -rf "$src"
  git init --quiet "$src"
  git -C "$src" fetch --quiet --depth 1 "$MVMD_REPO" "$MVMD_COMMIT"
  git -C "$src" checkout --quiet --detach FETCH_HEAD
fi
got=$(git -C "$src" rev-parse HEAD)
if [ "$got" != "$MVMD_COMMIT" ]; then
  echo "mvmd: the checkout is at $got, not the pinned $MVMD_COMMIT" >&2
  exit 1
fi
