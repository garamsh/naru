#!/bin/bash
# Builds the Node's initramfs (ADR 0034, 0054): node/cmd/naru-init as /init, the stand-in service
# the tests configure, naru-vip and naru-fence, mke2fs as mkfs.ext4 (ADR 0011), mkfs.xfs (ADR 0027),
# etcd, the two credential helpers and the public root bundle (ADR 0026), and Kubernetes with its
# container runtime, CNI plugins, nft, mount and umount, as initramfs.list lists them. The list is
# this directory's own and is not derived from the host's. Every program is a static executable
# (the image has no libc), which this script checks. The archive is a newc cpio written by the
# kernel's own usr/gen_init_cpio, which takes the entries in the order of initramfs.list, owns them
# by 0:0 and stamps every mtime with SOURCE_DATE_EPOCH; it is compressed with gzip -9n, which
# stores no name or time. The same inputs therefore give the same bytes (image/build.env fixes the
# environment).
#
# NARU_VERSION (optional, in the environment) is the version naru-init reports on the Node API.
#
# Usage: build.sh <build-dir> <kernel-build-dir> <output-dir> <e2fsprogs-dir> <etcd-dir> <regtools-dir> <xfsprogs-dir> <k8s-dir>
# <build-dir> receives the built programs, <kernel-build-dir> is the one image/host/kernel/build.sh
# was given, whose dl/ holds the verified kernel tarball (gen_init_cpio.c is taken from it),
# <e2fsprogs-dir> is the one image/common/e2fsprogs/build.sh wrote, <etcd-dir> the one
# image/node/etcd/build.sh wrote, <regtools-dir> holds what image/common/ecr-login/build.sh,
# image/common/rolesanywhere/build.sh and image/common/cacert/build.sh wrote, <xfsprogs-dir> is the
# one image/common/xfsprogs/build.sh wrote, <k8s-dir> holds what image/node/kubernetes/build.sh,
# image/node/containerd/build.sh, image/node/cni/build.sh, image/common/nftables/build.sh and
# image/common/util-linux/build.sh wrote: the Kubernetes programs, containerd, its shim, runc, the
# CNI plugins, nft, mount and umount. <output-dir> receives node-initrd.cpio.gz.
set -euo pipefail

if [ "$#" -ne 8 ]; then
  echo "usage: $0 <build-dir> <kernel-build-dir> <output-dir> <e2fsprogs-dir> <etcd-dir> <regtools-dir> <xfsprogs-dir> <k8s-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
mkdir -p "$1" "$3"
build=$(cd "$1" && pwd)
out=$(cd "$3" && pwd)
e2fsprogs=$(cd "$4" && pwd)
etcd=$(cd "$5" && pwd)
regtools=$(cd "$6" && pwd)
xfsprogs=$(cd "$7" && pwd)
k8s=$(cd "$8" && pwd)
# shellcheck source=../../build.env
. "$here/../../build.env"
# shellcheck source=../../common/kernel/pin.env
. "$here/../../common/kernel/pin.env"
# shellcheck source=../../common/initramfs/lib.sh
. "$here/../../common/initramfs/lib.sh"

bin=$build/bin
rm -rf "$bin"
mkdir -p "$bin"

# NARU_VERSION, when set, is the version naru-init reports on the Node API. The same value gives the
# same bytes: it is the only thing that enters the binary.
gobuild "$root/node" "$bin/naru-init" ./cmd/naru-init "${NARU_VERSION:+-X github.com/garamsh/naru/lib/init/run.buildVersion=$NARU_VERSION}"
gobuild "$root/tooling/qemu" "$bin/naru-standin" ./cmd/naru-standin
gobuild "$root/node" "$bin/naru-vip" ./cmd/naru-vip
gobuild "$root/node" "$bin/naru-fence" ./cmd/naru-fence

cp "$e2fsprogs/mke2fs" "$bin/mkfs.ext4"
cp "$xfsprogs/mkfs.xfs" "$bin/mkfs.xfs"
cp "$etcd/etcd" "$bin/etcd"
cp "$regtools/docker-credential-ecr-login" "$regtools/aws_signing_helper" "$bin/"
cp "$regtools/ca-certificates.crt" "$bin/ca-certificates.crt"
k8s_programs="kube-apiserver kube-controller-manager kube-scheduler kubelet kube-proxy containerd containerd-shim-runc-v2 runc nft mount umount bridge host-local loopback"
for f in $k8s_programs; do
  cp "$k8s/$f" "$bin/$f"
done

# shellcheck disable=SC2086
require_static "$bin" naru-init mkfs.ext4 mkfs.xfs naru-standin naru-vip naru-fence etcd docker-credential-ecr-login aws_signing_helper $k8s_programs

build_gen_init_cpio "$2" "$build/tools"
sed "s#@OUT@#$bin#" "$here/initramfs.list" >"$build/node.list"
"$build/tools/gen_init_cpio" -t "$SOURCE_DATE_EPOCH" "$build/node.list" | gzip -9n >"$out/node-initrd.cpio.gz"
echo "initramfs: built $out/node-initrd.cpio.gz, SHA-256 $(sha256sum "$out/node-initrd.cpio.gz" | cut -d' ' -f1)"
