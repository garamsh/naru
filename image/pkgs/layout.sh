#!/bin/bash
# Lays out the image directory the QEMU tests, the disk assembly and the release read (the Makefile's
# $(IMAGE)) from the packages of image/pkgs, which naru-pkg unpacks (docs/architecture/image.md §The
# package graph, ADR 0075). The packages hold what was built; this script only places files:
#
#   bzImage bzImage.bare initramfs.cpio.gz node-initrd.cpio.gz boot-sets.json inputs.env
#   guest/        guest.json, the guest kernel, the Node's boot-set kernel and the two guest initrds
#   extensions/   the naru-orch extension
#   gaon/ etcd/ k8s/ regtools/   the programs the tests run on the host or hand to the release
#
# regtools/ca-certificates.crt is the cacert package's bundle with the throwaway root of NARU_TEST_ROOT
# appended when the image is a test image (ADR 0026), the bytes the initramfs packages hold.
#
# Usage: layout.sh <naru-pkg> <pkgs-dir> <unpack-dir> <image-dir>
set -euo pipefail

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <naru-pkg> <pkgs-dir> <unpack-dir> <image-dir>" >&2
  exit 2
fi
naru_pkg=$1
pkgs=$2
root=$3
image=$4

want=(mvmd gaon etcd kubernetes containerd cni ecr-login rolesanywhere cacert extension kernel-host initramfs-host initramfs-node bzImage)
tars=()
for p in "${want[@]}"; do
  tars+=("$pkgs/$p.tar")
done
# Only what this script places is replaced: the disk step writes naru.img and naru.qcow2 into the same
# directory, and make image-upgrade lays out again after it.
rm -rf "$root"
mkdir -p "$root" "$image"
for f in bzImage bzImage.bare initramfs.cpio.gz node-initrd.cpio.gz boot-sets.json inputs.env guest extensions gaon etcd k8s regtools; do
  rm -rf "${image:?}/$f"
done
"$naru_pkg" unpack "$root" "${tars[@]}"

cp "$root/bzImage/bzImage" "$root/kernel-host/bzImage.bare" "$root/initramfs-host/initramfs.cpio.gz" \
  "$root/initramfs-host/boot-sets.json" "$root/initramfs-node/node-initrd.cpio.gz" "$root/mvmd/inputs.env" "$image/"
cp -r "$root/initramfs-host/guest" "$image/guest"
cp -r "$root/extension/extensions" "$image/extensions"

mkdir -p "$image/gaon" "$image/etcd" "$image/k8s" "$image/regtools"
cp "$root"/gaon/usr/bin/* "$image/gaon/"
cp "$root/etcd/usr/bin/etcd" "$image/etcd/"
cp "$root"/kubernetes/usr/bin/* "$root"/containerd/usr/bin/* "$root"/cni/usr/bin/* "$image/k8s/"
cp "$root/ecr-login/usr/bin/docker-credential-ecr-login" "$root/rolesanywhere/usr/bin/aws_signing_helper" "$image/regtools/"
{
  cat "$root/cacert/etc/ssl/certs/ca-certificates.crt"
  if [ -n "${NARU_TEST_ROOT:-}" ]; then
    cat "$NARU_TEST_ROOT"
  fi
} >"$image/regtools/ca-certificates.crt"
