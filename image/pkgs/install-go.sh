#!/bin/bash
# Installs one Go program a package ships: fails unless it was built by the given Go release (`go
# version -m` reads it from the binary) and is a static executable, then copies it with mode 0755.
#
# Usage: install-go.sh <go-release> <built-program> <destination>
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <go-release> <built-program> <destination>" >&2
  exit 2
fi
got=$(go version -m "$2" | awk 'NR == 1 { print $2 }')
if [ "$got" != "$1" ]; then
  echo "install-go: $2 was built by ${got:-no Go release}, not the expected $1" >&2
  exit 1
fi
if ! file "$2" | grep -Eq 'statically linked|static-pie linked'; then
  echo "install-go: $2 is not a static executable: $(file "$2")" >&2
  exit 1
fi
mkdir -p "$(dirname "$3")"
install -m 0755 "$2" "$3"
echo "install-go: $3, SHA-256 $(sha256sum "$3" | cut -d' ' -f1)"
