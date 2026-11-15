# shellcheck shell=bash
# Functions the build scripts of image/ share, sourced and never run. Each script sources
# image/build.env first; the two initramfs builds also source image/common/kernel/pin.env before
# calling build_gen_init_cpio.

# build_current <stamp-file> <output>... -- <input>... sets STAMP to the SHA-256 of the input files,
# of this file and of the builder's name (NARU_BUILDER, which image/builder/run.sh sets), and
# returns 0 when <stamp-file> holds STAMP and every <output> exists: the outputs were built from
# these inputs and the caller skips the build. After a build the caller writes STAMP to
# <stamp-file>.
build_current() {
  local stamp_file=$1 outputs=() f
  shift
  while [ "$1" != -- ]; do
    outputs+=("$1")
    shift
  done
  shift
  STAMP=$(cat "$@" "${BASH_SOURCE[0]}" | sha256sum | cut -d' ' -f1)-${NARU_BUILDER:-outside-the-builder}
  [ -f "$stamp_file" ] && [ "$(cat "$stamp_file")" = "$STAMP" ] || return 1
  for f in "${outputs[@]}"; do
    [ -f "$f" ] || return 1
  done
}

# require_go <release> <file>... fails unless each file is a Go program built by <release> (go1.26.11;
# `go version -m` reads it from the binary). The programs of naru and of mvmd are checked against
# NARU_GO_VERSION, the builder's own release; a project that names its own release in its script
# (GOTOOLCHAIN) is checked against that one.
require_go() {
  local want=$1 f got
  shift
  for f in "$@"; do
    got=$(go version -m "$f" | awk 'NR == 1 { print $2 }')
    if [ "$got" != "$want" ]; then
      echo "image: $f was built by ${got:-no Go release}, not the expected $want" >&2
      return 1
    fi
  done
}

# nss_gate <name> <log> fails when the build log shows glibc's warning that a static program calls
# a function (getpwnam, getaddrinfo and the like) which needs the shared libraries of the glibc it
# was linked with at run time, unless <name> is on image/nss-allow.txt. The image has no libc, so
# such a call fails on the Node.
nss_gate() {
  local lib_dir
  lib_dir=$(dirname "${BASH_SOURCE[0]}")
  if grep -q 'statically linked applications requires at runtime the shared libraries' "$2" &&
    ! grep -Eq "^$1([[:space:]]|\$)" "$lib_dir/nss-allow.txt"; then
    grep 'statically linked applications requires at runtime the shared libraries' "$2" | sort -u >&2
    echo "image: $1 links glibc's NSS functions statically and is not on image/nss-allow.txt" >&2
    return 1
  fi
}

# gobuild <module-dir> <output> <package> [<extra ldflags>] builds one static Go program of this
# repository with the builder's Go release and checks the release the binary records. The same
# inputs give the same bytes: nothing but the flags and the sources enters it.
gobuild() {
  (cd "$1" && CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath -buildvcs=false -ldflags="-s -w ${4:-}" -o "$2" "$3")
  require_go "$NARU_GO_VERSION" "$2"
}

# require_static <dir> <name>... fails unless every named file of <dir> is a static executable: the
# image has no libc.
require_static() {
  local dir=$1 f
  shift
  for f in "$@"; do
    if ! file "$dir/$f" | grep -Eq 'statically linked|static-pie linked'; then
      echo "initramfs: $f is not a static executable: $(file "$dir/$f")" >&2
      return 1
    fi
  done
}

# build_gen_init_cpio <kernel-build-dir> <tools-dir> compiles the kernel's own usr/gen_init_cpio
# from the pinned tarball in <kernel-build-dir>/dl, after checking the tarball's SHA-256.
# KERNEL_URL, KERNEL_SHA256 and KERNEL_VERSION come from image/common/kernel/pin.env.
build_gen_init_cpio() {
  local tarball
  tarball=$(cd "$1" && pwd)/dl/${KERNEL_URL##*/}
  if [ "$(sha256sum "$tarball" | cut -d' ' -f1)" != "$KERNEL_SHA256" ]; then
    echo "initramfs: $tarball does not have the pinned kernel SHA-256" >&2
    return 1
  fi
  mkdir -p "$2"
  tar -xf "$tarball" -C "$2" --strip-components=2 "linux-$KERNEL_VERSION/usr/gen_init_cpio.c"
  gcc -O2 -o "$2/gen_init_cpio" "$2/gen_init_cpio.c"
}
