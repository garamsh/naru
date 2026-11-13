#!/bin/bash
# Builds the initramfs: naru-init as /init, mvmd and mvmd-supervisor from the pinned mvmd commit,
# the Firecracker binary mvmd's fetch.sh verifies, mke2fs as mkfs.ext4 (ADR 0011), mkfs.xfs (ADR 0027), the gaon
# services and etcd (ADR 0012), the two credential helpers and the public root bundle (ADR 0026), the stand-in
# service the tests configure, Kubernetes with its container runtime, CNI plugins, nft, mount and umount, and the guest kernels, the two initrds and guest.json that image/guest/build.sh wrote, which a Machine
# boots on (ADR 0010).
# Every program is a static executable (the image has no libc), which this script checks. The archive is a newc cpio written by the kernel's own
# usr/gen_init_cpio, which takes the entries in the order of initramfs.list, owns them by 0:0 and
# stamps every mtime with SOURCE_DATE_EPOCH; it is compressed with gzip -9n, which stores no name
# or time. The same inputs therefore give the same bytes (image/build.env fixes the environment).
#
# NARU_VERSION (optional, in the environment) is the version naru-init reports on the Node API.
#
# Usage: build.sh <build-dir> <kernel-build-dir> <output-dir> <guest-dir> <e2fsprogs-dir> <gaon-dir> <etcd-dir> <regtools-dir> <xfsprogs-dir> <k8s-dir>
# <build-dir> receives the mvmd checkout and the built programs, <kernel-build-dir> is the one
# image/kernel/build.sh was given, whose dl/ holds the verified kernel tarball (gen_init_cpio.c is
# taken from it), <guest-dir> is the one image/guest/build.sh wrote, <e2fsprogs-dir> is the one
# image/e2fsprogs/build.sh wrote, <gaon-dir> and <etcd-dir> are the ones image/gaon/build.sh and
# image/etcd/build.sh wrote, <regtools-dir> holds what image/ecr-login/build.sh,
# image/rolesanywhere/build.sh and image/cacert/build.sh wrote, <xfsprogs-dir> is the one image/xfsprogs/build.sh wrote, and <output-dir> receives
# initramfs.cpio.gz and inputs.env, which names the Firecracker binary mvmd's fetch.sh yields.
# <k8s-dir> holds what image/kubernetes/build.sh, image/containerd/build.sh, image/cni/build.sh,
# image/nftables/build.sh and image/util-linux/build.sh wrote: the Kubernetes programs, containerd,
# its shim, runc, the CNI plugins, nft, mount and umount.
set -euo pipefail

if [ "$#" -ne 10 ]; then
  echo "usage: $0 <build-dir> <kernel-build-dir> <output-dir> <guest-dir> <e2fsprogs-dir> <gaon-dir> <etcd-dir> <regtools-dir> <xfsprogs-dir> <k8s-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
mkdir -p "$1" "$3"
build=$(cd "$1" && pwd)
out=$(cd "$3" && pwd)
guest=$(cd "$4" && pwd)
e2fsprogs=$(cd "$5" && pwd)
gaon=$(cd "$6" && pwd)
etcd=$(cd "$7" && pwd)
regtools=$(cd "$8" && pwd)
xfsprogs=$(cd "$9" && pwd)
k8s=$(cd "${10}" && pwd)
# shellcheck source=pin.env
. "$here/pin.env"
# shellcheck source=../build.env
. "$here/../build.env"
# shellcheck source=../kernel/pin.env
. "$here/../kernel/pin.env"
# shellcheck source=../guest/kernels.env
. "$here/../guest/kernels.env"
tarball=$(cd "$2" && pwd)/dl/${KERNEL_URL##*/}

if [ "$(sha256sum "$tarball" | cut -d' ' -f1)" != "$KERNEL_SHA256" ]; then
  echo "initramfs: $tarball does not have the pinned kernel SHA-256" >&2
  exit 1
fi

bin=$build/bin
rm -rf "$bin"
mkdir -p "$bin"

# naru-init and the stand-in are built from this repository.
gobuild() { # <module-dir> <output> <package> [<extra ldflags>]
  (cd "$1" && CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath -buildvcs=false -ldflags="-s -w ${4:-}" -o "$2" "$3")
}
# NARU_VERSION, when set, is the version naru-init reports on the Node API. The same value gives the
# same bytes: it is the only thing that enters the binary.
gobuild "$root/init" "$bin/naru-init" ./cmd/naru-init "${NARU_VERSION:+-X main.buildVersion=$NARU_VERSION}"
gobuild "$root/tooling/qemu" "$bin/naru-standin" ./cmd/naru-standin
gobuild "$root/controlplane" "$bin/naru-vip" ./cmd/naru-vip
gobuild "$root/controlplane" "$bin/naru-fence" ./cmd/naru-fence
gobuild "$root/host" "$bin/naru-hostd" ./cmd/naru-hostd

# mvmd is fetched at its pinned commit and nowhere else.
src=$build/mvmd
"$here/../mvmd-checkout.sh" "$src"
(cd "$src" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$bin/mvmd" ./daemon/cmd/mvmd)
(cd "$src" && CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "$bin/mvmd-supervisor" ./daemon/cmd/mvmd-supervisor)

# Firecracker, from mvmd's own checked fetch. --firecracker-only fetches and verifies the
# Firecracker binary alone: no guest kernel (which naru does not use, ADR 0010) is requested, and
# inputs.env names only MVMD_FIRECRACKER and MVMD_FIRECRACKER_SHA256, which the tests read.
"$src/tooling/firecracker/fetch.sh" --firecracker-only "$build/inputs" >/dev/null
cp "$build/inputs/firecracker" "$bin/firecracker"
cp "$e2fsprogs/mke2fs" "$bin/mkfs.ext4"
cp "$xfsprogs/mkfs.xfs" "$bin/mkfs.xfs"
for f in gaon-node-agent gaon-apiserver gaon-scheduler gaon-controllers; do
  cp "$gaon/$f" "$bin/$f"
done
cp "$etcd/etcd" "$bin/etcd"
cp "$regtools/docker-credential-ecr-login" "$regtools/aws_signing_helper" "$bin/"
cp "$regtools/ca-certificates.crt" "$bin/ca-certificates.crt"
k8s_programs="kube-apiserver kube-controller-manager kube-scheduler kubelet kube-proxy containerd containerd-shim-runc-v2 runc nft mount umount bridge host-local loopback"
for f in $k8s_programs; do
  cp "$k8s/$f" "$bin/$f"
done
sed "s#=$build/inputs/#=$out/#" "$build/inputs/inputs.env" >"$out/inputs.env"

for f in naru-init mvmd mvmd-supervisor firecracker mkfs.ext4 mkfs.xfs naru-standin naru-vip naru-fence naru-hostd gaon-node-agent gaon-apiserver gaon-scheduler gaon-controllers etcd docker-credential-ecr-login aws_signing_helper $k8s_programs; do
  if ! file "$bin/$f" | grep -Eq 'statically linked|static-pie linked'; then
    echo "initramfs: $f is not a static executable: $(file "$bin/$f")" >&2
    exit 1
  fi
done

# gen_init_cpio is the kernel's own, compiled from the pinned tarball.
tools=$build/tools
mkdir -p "$tools"
tar -xf "$tarball" -C "$tools" --strip-components=2 "linux-$KERNEL_VERSION/usr/gen_init_cpio.c"
gcc -O2 -o "$tools/gen_init_cpio" "$tools/gen_init_cpio.c"

# The guest files go under GUEST_IMAGE_DIR, which guest.json's paths name: every directory down to
# it, then each kernel's directory, then the files, in a fixed order.
{
  sed "s#@OUT@#$bin#" "$here/initramfs.list"
  acc=
  IFS=/ read -ra parts <<<"${GUEST_IMAGE_DIR#/}"
  for part in "${parts[@]}"; do
    acc="$acc/$part"
    echo "dir $acc 0755 0 0"
  done
  echo "dir $GUEST_IMAGE_DIR/kernels 0755 0 0"
  for name in $GUEST_KERNELS; do
    echo "dir $GUEST_IMAGE_DIR/kernels/$name 0755 0 0"
  done
  for name in $GUEST_KERNELS; do
    echo "file $GUEST_IMAGE_DIR/kernels/$name/vmlinux $guest/kernels/$name/vmlinux 0644 0 0"
  done
  for f in guest-initrd.cpio converter-initrd.cpio guest.json; do
    echo "file $GUEST_IMAGE_DIR/$f $guest/$f 0644 0 0"
  done
} >"$build/initramfs.list"
"$tools/gen_init_cpio" -t "$SOURCE_DATE_EPOCH" "$build/initramfs.list" >"$build/initramfs.cpio"
gzip -9n <"$build/initramfs.cpio" >"$out/initramfs.cpio.gz"
echo "initramfs: built $out/initramfs.cpio.gz, SHA-256 $(sha256sum "$out/initramfs.cpio.gz" | cut -d' ' -f1)"
