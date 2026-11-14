# Functions the two initramfs builds share (image/host/initramfs/build.sh and
# image/node/initramfs/build.sh), sourced and never run (ADR 0054). Each build sources
# image/build.env and image/common/kernel/pin.env before calling them.

# gobuild <module-dir> <output> <package> [<extra ldflags>] builds one static Go program of this
# repository. The same inputs give the same bytes: nothing but the flags and the sources enters it.
gobuild() {
  (cd "$1" && CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath -buildvcs=false -ldflags="-s -w ${4:-}" -o "$2" "$3")
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
