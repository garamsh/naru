#!/bin/bash
# Installs one program a package ships: fails unless the linked program holds no libc iconv (musl
# 1.2.6's iconv has a published defect no release fixes yet, CVE-2026-6042, which no naru program
# may reach), strips it, then fails unless it is a static executable built by the toolchain, not
# by the builder's gcc (its .comment names no distribution compiler).
#
# Usage: install-bin.sh <linked-program> <destination>
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <linked-program> <destination>" >&2
  exit 2
fi
if nm "$1" | grep -Eq ' [TtWw] (iconv|iconv_open|iconv_close)$'; then
  echo "install-bin: $1 links musl's iconv" >&2
  exit 1
fi
mkdir -p "$(dirname "$2")"
x86_64-linux-musl-strip --strip-all -o "$2" "$1"
chmod 0755 "$2"
if ! file "$2" | grep -Eq 'statically linked|static-pie linked'; then
  echo "install-bin: $2 is not a static executable: $(file "$2")" >&2
  exit 1
fi
if readelf -p .comment "$2" | grep -q Ubuntu; then
  echo "install-bin: $2 was compiled by the builder's gcc, not the toolchain" >&2
  exit 1
fi
echo "install-bin: $2, SHA-256 $(sha256sum "$2" | cut -d' ' -f1)"
