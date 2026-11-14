#!/bin/bash
# Builds the host image's initramfs: host/cmd/naru-init as /init, naru-hostd (naru-signer and
# naru-orchd are in the naru-orch extension, image/host/extension/build.sh, ADR 0055), mvmd and mvmd-supervisor from the pinned mvmd commit, the Firecracker
# binary mvmd's fetch.sh verifies, gaon's services, and what the boot disk's own Node form runs
# (ADR 0007, 0031, 0054): the stand-in service the tests configure, naru-vip and naru-fence, mke2fs
# as mkfs.ext4 (ADR 0011), mkfs.xfs (ADR 0027), etcd, the two credential helpers and the public
# root bundle (ADR 0026), Kubernetes with its container runtime, CNI plugins, nft, mount and
# umount. It also carries the guest kernels, the two initrds and guest.json that
# image/host/guest/build.sh wrote, which a Machine boots on (ADR 0010), and the Node's microVM form
# (ADR 0034): the kernel image/node/kernel/build.sh wrote and the initrd
# image/node/initramfs/build.sh wrote into <output-dir>, with boot-sets.json, which names both.
# Every program is a static executable (the image has no libc), which this script checks. The
# archive is a newc cpio written by the kernel's own usr/gen_init_cpio, which takes the entries in
# the order of initramfs.list, owns them by 0:0 and stamps every mtime with SOURCE_DATE_EPOCH; it is
# compressed with gzip -9n, which stores no name or time. The same inputs therefore give the same
# bytes (image/build.env fixes the environment).
#
# NARU_VERSION (optional, in the environment) is the version naru-init reports on the Node API.
#
# Usage: build.sh <build-dir> <kernel-build-dir> <output-dir> <guest-dir> <e2fsprogs-dir> <gaon-dir> <etcd-dir> <regtools-dir> <xfsprogs-dir> <k8s-dir>
# <build-dir> receives the mvmd checkout and the built programs, <kernel-build-dir> is the one
# image/host/kernel/build.sh was given, whose dl/ holds the verified kernel tarball (gen_init_cpio.c
# is taken from it), <guest-dir> is the one image/host/guest/build.sh and image/node/kernel/build.sh
# wrote, <e2fsprogs-dir> is the one image/common/e2fsprogs/build.sh wrote, <gaon-dir> and
# <etcd-dir> are the ones image/host/gaon/build.sh and image/node/etcd/build.sh wrote,
# <regtools-dir> holds what image/common/ecr-login/build.sh, image/common/rolesanywhere/build.sh
# and image/common/cacert/build.sh wrote, <xfsprogs-dir> is the one image/common/xfsprogs/build.sh
# wrote, and <k8s-dir> holds what image/node/kubernetes/build.sh, image/node/containerd/build.sh,
# image/node/cni/build.sh, image/common/nftables/build.sh and image/common/util-linux/build.sh
# wrote: the Kubernetes programs, containerd, its shim, runc, the CNI plugins, nft, mount and
# umount. <output-dir> holds node-initrd.cpio.gz, which image/node/initramfs/build.sh wrote, and
# receives initramfs.cpio.gz, boot-sets.json (already inside initramfs.cpio.gz) and inputs.env,
# which names the Firecracker binary mvmd's fetch.sh yields.
set -euo pipefail

if [ "$#" -ne 10 ]; then
  echo "usage: $0 <build-dir> <kernel-build-dir> <output-dir> <guest-dir> <e2fsprogs-dir> <gaon-dir> <etcd-dir> <regtools-dir> <xfsprogs-dir> <k8s-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../../.." && pwd)
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
# shellcheck source=../mvmd/pin.env
. "$here/../mvmd/pin.env"
# shellcheck source=../../build.env
. "$here/../../build.env"
# shellcheck source=../../common/kernel/pin.env
. "$here/../../common/kernel/pin.env"
# shellcheck source=../../common/initramfs/lib.sh
. "$here/../../common/initramfs/lib.sh"
# shellcheck source=../../node/boot-set.env
. "$here/../../node/boot-set.env"
# shellcheck source=../guest/kernels.env
. "$here/../guest/kernels.env"
node_initrd=$out/node-initrd.cpio.gz
if [ ! -f "$node_initrd" ]; then
  echo "initramfs: $node_initrd is missing; image/node/initramfs/build.sh writes it" >&2
  exit 1
fi

bin=$build/bin
rm -rf "$bin"
mkdir -p "$bin"

# NARU_VERSION, when set, is the version naru-init reports on the Node API. The same value gives the
# same bytes: it is the only thing that enters the binary. The boot disk's own Node form runs
# naru-vip, naru-fence and the stand-in, so they are built here as the Node's initramfs builds them.
gobuild "$root/host" "$bin/naru-init" ./cmd/naru-init "${NARU_VERSION:+-X github.com/garamsh/naru/lib/init/run.buildVersion=$NARU_VERSION}"
gobuild "$root/tooling/qemu" "$bin/naru-standin" ./cmd/naru-standin
gobuild "$root/node" "$bin/naru-vip" ./cmd/naru-vip
gobuild "$root/node" "$bin/naru-fence" ./cmd/naru-fence
gobuild "$root/host" "$bin/naru-hostd" ./cmd/naru-hostd

# mvmd is fetched at its pinned commit and nowhere else.
src=$build/mvmd
"$here/../mvmd/checkout.sh" "$src"
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

# shellcheck disable=SC2086
require_static "$bin" naru-init mvmd mvmd-supervisor firecracker mkfs.ext4 mkfs.xfs naru-standin naru-vip naru-fence naru-hostd gaon-node-agent gaon-apiserver gaon-scheduler gaon-controllers etcd docker-credential-ecr-login aws_signing_helper $k8s_programs

build_gen_init_cpio "$2" "$build/tools"
tools=$build/tools

# The boot set mvmd's configuration names (mvmd ADR 0058): the guest form's kernel and initrd, each
# with the path it has inside the image and its SHA-256.
boot_dir=$GUEST_IMAGE_DIR/boot-sets/$GUEST_BOOT_SET
sha_of() { sha256sum "$1" | cut -d' ' -f1; }
printf '{\n  "boot_sets": {\n    "%s": {\n      "kernel": {"path": "%s/vmlinux", "sha256": "%s"},\n      "initrd": {"path": "%s/initrd.cpio.gz", "sha256": "%s"}\n    }\n  }\n}\n' \
  "$GUEST_BOOT_SET" "$boot_dir" "$(sha_of "$guest/boot-sets/$GUEST_BOOT_SET/vmlinux")" "$boot_dir" "$(sha_of "$node_initrd")" >"$build/boot-sets.json"

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
  echo "dir $GUEST_IMAGE_DIR/boot-sets 0755 0 0"
  echo "dir $boot_dir 0755 0 0"
  for name in $GUEST_KERNELS; do
    echo "file $GUEST_IMAGE_DIR/kernels/$name/vmlinux $guest/kernels/$name/vmlinux 0644 0 0"
  done
  echo "file $boot_dir/vmlinux $guest/boot-sets/$GUEST_BOOT_SET/vmlinux 0644 0 0"
  echo "file $boot_dir/initrd.cpio.gz $node_initrd 0644 0 0"
  for f in guest-initrd.cpio converter-initrd.cpio guest.json; do
    echo "file $GUEST_IMAGE_DIR/$f $guest/$f 0644 0 0"
  done
  echo "file $GUEST_IMAGE_DIR/boot-sets.json $build/boot-sets.json 0644 0 0"
} >"$build/initramfs.list"
"$tools/gen_init_cpio" -t "$SOURCE_DATE_EPOCH" "$build/initramfs.list" >"$build/initramfs.cpio"
gzip -9n <"$build/initramfs.cpio" >"$out/initramfs.cpio.gz"
cp "$build/boot-sets.json" "$out/"
echo "initramfs: built $out/initramfs.cpio.gz, SHA-256 $(sha256sum "$out/initramfs.cpio.gz" | cut -d' ' -f1)"
