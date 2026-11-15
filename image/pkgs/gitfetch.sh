#!/bin/sh
# Fetches one commit of a git repository into <dir> and fails unless HEAD is that commit: the only
# way a package's Dockerfile gets a source that is a git checkout (docs/architecture/image.md
# §The package graph). The commit, not a tag, is the pin: a tag can be moved.
#
# Usage: gitfetch.sh <repo> <commit> <dir>
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <repo> <commit> <dir>" >&2
  exit 2
fi
git init --quiet "$3"
git -C "$3" fetch --quiet --depth 1 "$1" "$2"
git -C "$3" checkout --quiet --detach FETCH_HEAD
got=$(git -C "$3" rev-parse HEAD)
if [ "$got" != "$2" ]; then
  echo "gitfetch: the checkout of $1 is at $got, not the pinned $2" >&2
  exit 1
fi
echo "gitfetch: $1 is at the pinned $2"
