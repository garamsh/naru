#!/bin/sh
# Prints the full commit of the mvmd the image carries: the one the Go modules of this repository
# pin. host/go.mod's version of the module that image/pkgs/mvmd/pin.env's MVMD_COMMIT_FROM names
# (mvmd's runtimeapi) is a pseudo-version of an mvmd commit, and go resolves it to the whole hash.
# The release's component list reads the commit the same way (tooling/qemu/internal/release). That version is the only place the commit is
# written (docs/architecture/image.md §The package graph), so the daemon the image runs and the
# client hostd is compiled against cannot differ. It fails when another module of the workspace
# pins another version of it.
#
# Usage: mvmd-commit.sh
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
mod=$(sed -n 's/^MVMD_COMMIT_FROM=//p' "$root/image/pkgs/mvmd/pin.env")
version() { (cd "$root/$1" && GOWORK=off go list -m -f '{{.Version}}' "$mod"); }
want=$(version host)
for m in tooling/qemu tooling/probe; do
  got=$(version "$m")
  if [ "$got" != "$want" ]; then
    echo "mvmd-commit: $m pins $mod $got and host pins $want; one mvmd commit is pinned" >&2
    exit 1
  fi
done
hash=$(cd "$root/host" && GOWORK=off GOPROXY="${GOPROXY:-https://proxy.golang.org,direct}" go list -m -json "$mod@$want" |
  awk -F'"' '/"Hash":/ { print $4; exit }')
if [ "${#hash}" -ne 40 ]; then
  echo "mvmd-commit: go did not resolve $mod@$want to a commit" >&2
  exit 1
fi
echo "$hash"
