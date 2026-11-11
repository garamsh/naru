#!/bin/bash
# Builds the UEFI disk image from the kernel and the initramfs that image/kernel/build.sh and
# image/initramfs/build.sh wrote: naru.img, a raw GPT disk whose one EFI system partition holds
# the kernel as \EFI\BOOT\BOOTX64.EFI and the initramfs as \INITRD.GZ, and naru.qcow2, the same
# disk as an uncompressed qcow2 for Proxmox's import_from. The disk is written by tooling/disk
# with mkfs.fat and sgdisk; nothing needs root or mtools. Every identifier is fixed by disk.env and
# image/build.env, so the same inputs give the same bytes.
#
# Usage: build.sh <build-dir> <image-dir>
# <build-dir> receives the naru-disk binary; <image-dir> holds bzImage and initramfs.cpio.gz and
# receives naru.img and naru.qcow2.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <build-dir> <image-dir>" >&2
  exit 2
fi
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
mkdir -p "$1"
build=$(cd "$1" && pwd)
img=$(cd "$2" && pwd)
# shellcheck source=disk.env
. "$here/disk.env"
# shellcheck source=../build.env
. "$here/../build.env"

for f in bzImage initramfs.cpio.gz; do
  if [ ! -f "$img/$f" ]; then
    echo "disk: $img/$f is missing; make image builds it" >&2
    exit 1
  fi
done

(cd "$root/tooling/disk" && CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath -buildvcs=false -o "$build/naru-disk" ./cmd/naru-disk)

rm -f "$img/naru.img.part" "$img/naru.qcow2.part"
"$build/naru-disk" -kernel "$img/bzImage" -initrd "$img/initramfs.cpio.gz" -out "$img/naru.img.part" \
  -disk-guid "$DISK_GUID" -esp-guid "$ESP_GUID"
mv "$img/naru.img.part" "$img/naru.img"
qemu-img convert -f raw -O qcow2 "$img/naru.img" "$img/naru.qcow2.part"
mv "$img/naru.qcow2.part" "$img/naru.qcow2"
echo "disk: built $img/naru.img, SHA-256 $(sha256sum "$img/naru.img" | cut -d' ' -f1)"
echo "disk: built $img/naru.qcow2, SHA-256 $(sha256sum "$img/naru.qcow2" | cut -d' ' -f1)"
