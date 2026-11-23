#!/bin/bash
# Writes boot-sets.json and guest.json and the initramfs list of the Host's initramfs, then the archive,
# from what the Dockerfile has put in /stage and /guest (image/pkgs/initramfs-host/Dockerfile). The
# JSON names each guest file with the path it has inside the image and its SHA-256: guest.json is
# the kernels as the `kernels` map and default_kernel of mvmd's driver configuration
# (garamsh/mvmd#364) and the two initrds, boot-sets.json the boot set mvmd's configuration names
# (mvmd ADR 0058): the Node's kernel and initrd.
set -euo pipefail

# shellcheck source=kernels.env
. /kernels.env
# shellcheck source=boot-set.env
. /boot-set.env

if ! [[ "$GUEST_BOOT_SET" =~ ^[a-z0-9-]{1,63}$ ]]; then
  echo "initramfs: GUEST_BOOT_SET $GUEST_BOOT_SET is not 1 to 63 characters of [a-z0-9-]" >&2
  exit 1
fi
default_listed=0
for name in $GUEST_KERNELS; do
  if ! [[ "$name" =~ ^[a-z0-9-]{1,63}$ ]]; then
    echo "initramfs: the guest kernel $name is not 1 to 63 characters of [a-z0-9-]" >&2
    exit 1
  fi
  if [ ! -f "/guest/kernels/$name/vmlinux" ]; then
    echo "initramfs: GUEST_KERNELS names $name, whose vmlinux the Dockerfile does not copy" >&2
    exit 1
  fi
  if [ "$name" = "$GUEST_DEFAULT_KERNEL" ]; then
    default_listed=1
  fi
done
if [ "$default_listed" -ne 1 ]; then
  echo "initramfs: GUEST_DEFAULT_KERNEL $GUEST_DEFAULT_KERNEL is not one of GUEST_KERNELS ($GUEST_KERNELS)" >&2
  exit 1
fi

sha_of() { sha256sum "$1" | cut -d' ' -f1; }
boot_dir=$GUEST_IMAGE_DIR/boot-sets/$GUEST_BOOT_SET

printf '{\n  "boot_sets": {\n    "%s": {\n      "kernel": {"path": "%s/vmlinux", "sha256": "%s"},\n      "initrd": {"path": "%s/initrd.cpio.gz", "sha256": "%s"}\n    }\n  }\n}\n' \
  "$GUEST_BOOT_SET" "$boot_dir" "$(sha_of "/guest/boot-sets/$GUEST_BOOT_SET/vmlinux")" "$boot_dir" "$(sha_of /node-initrd.cpio.gz)" >/boot-sets.json

{
  printf '{\n  "kernels": {'
  sep=
  for name in $GUEST_KERNELS; do
    printf '%s\n    "%s": {"path": "%s/kernels/%s/vmlinux", "sha256": "%s"}' \
      "$sep" "$name" "$GUEST_IMAGE_DIR" "$name" "$(sha_of "/guest/kernels/$name/vmlinux")"
    sep=,
  done
  printf '\n  },\n  "default_kernel": "%s",\n' "$GUEST_DEFAULT_KERNEL"
  printf '  "guest_initrd": {"path": "%s/guest-initrd.cpio", "sha256": "%s"},\n' "$GUEST_IMAGE_DIR" "$(sha_of /guest/guest-initrd.cpio)"
  printf '  "converter_initrd": {"path": "%s/converter-initrd.cpio", "sha256": "%s"}\n}\n' "$GUEST_IMAGE_DIR" "$(sha_of /guest/converter-initrd.cpio)"
} >/guest/guest.json

# The guest files go under GUEST_IMAGE_DIR, which guest.json's paths name: every directory down to it,
# then each kernel's directory, then the files, in a fixed order. The kernel's unpacker creates no
# parent directory and skips an entry whose parent it has not unpacked, without a message, so the list
# is ordered with every directory before its entries and then checked, and a list that is not fails the
# build (garamsh/naru#356).
{
  sed "s#@OUT@#/stage#" /initramfs.list
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
    echo "file $GUEST_IMAGE_DIR/kernels/$name/vmlinux /guest/kernels/$name/vmlinux 0644 0 0"
  done
  echo "file $boot_dir/vmlinux /guest/boot-sets/$GUEST_BOOT_SET/vmlinux 0644 0 0"
  echo "file $boot_dir/initrd.cpio.gz /node-initrd.cpio.gz 0644 0 0"
  for f in guest-initrd.cpio converter-initrd.cpio guest.json; do
    echo "file $GUEST_IMAGE_DIR/$f /guest/$f 0644 0 0"
  done
  echo "file $GUEST_IMAGE_DIR/boot-sets.json /boot-sets.json 0644 0 0"
} >/host.raw.list
naru-cpiolist order /host.raw.list >/host.list
naru-cpiolist check /host.list

mkdir -p /out
gen_init_cpio -t "$SOURCE_DATE_EPOCH" /host.list | gzip -9n >/out/initramfs.cpio.gz
cp -r /guest /boot-sets.json /out/
echo "initramfs: built initramfs.cpio.gz, SHA-256 $(sha_of /out/initramfs.cpio.gz)"
