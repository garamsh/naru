# ADR 0007: The Proxmox boot form is a UEFI disk image whose EFI-stub kernel loads the initramfs from the ESP

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#15. It stands beside [ADR 0004](./0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md), which is not edited: ADR 0004's direct boot (`-kernel`, `-initrd`) stays the form of the `TestNode_*` tests that need no firmware, and this ADR adds the form a Proxmox host can attach. Claims about the Linux kernel hold for the longterm 6.18.63 that `image/kernel/pin.env` pins.

## Context

- **Proxmox does not pass `-kernel`.** It attaches disks to a VM with OVMF firmware, and imports a disk as qcow2 (garamsh/naru#14), so the image must boot from a disk on its own.
- **The kernel can be its own UEFI loader.** `Documentation/admin-guide/efi-stub.rst` describes the EFI stub: with `CONFIG_EFI_STUB` the bzImage is a PE/COFF executable that firmware starts from an ESP as `\EFI\BOOT\BOOTX64.EFI`, so no bootloader is needed.
- **The initramfs has two ways in.** Embedded in the kernel (`CONFIG_INITRAMFS_SOURCE`), or loaded by the stub from the ESP. `efi-stub.rst` documents the second: an `initrd=` option on the command line names a file on the volume the stub was loaded from.
- **The command line has no bootloader to carry it.** The same document says the stub takes it from `CONFIG_CMDLINE` when the firmware passes none; `arch/x86/Kconfig` defines `CMDLINE_BOOL` and `CMDLINE`, and `arch/x86/kernel/setup.c` prepends the built-in line to the boot loader's unless `CMDLINE_OVERRIDE` is set. In `drivers/firmware/efi/libstub/x86-stub.c` the stub parses the built-in line too, so `initrd=` in it is honoured. `libstub/file.c` turns `/` into `\` and strips leading separators, so `initrd=/INITRD.GZ` names `\INITRD.GZ`.
- **The environment has no root and no mtools or xorriso.** `/sbin/mkfs.fat` (dosfstools) and `/sbin/sgdisk` (gdisk) are present and write a FAT volume and a GPT without mounting.

## Decision

**The Proxmox boot form is `build/image/naru.img`, a raw GPT disk with one EFI system partition holding the kernel as `\EFI\BOOT\BOOTX64.EFI` and the initramfs as `\INITRD.GZ`, built by `make disk` without root.**

1. **The initramfs is loaded by the stub from the ESP, not embedded.** The kernel is built with `CONFIG_EFI_STUB`, `CONFIG_CMDLINE_BOOL` and `CONFIG_CMDLINE="console=ttyS0 panic=-1 loglevel=4 initrd=/INITRD.GZ"`. One bzImage then serves the disk and the direct-boot tests, whose `-append` is appended to the built-in line, and the kernel build still precedes the initramfs build.
2. **The disk is written by `tooling/disk`.** `mkfs.fat` makes an empty FAT32 volume and `sgdisk` writes the GPT; `tooling/disk/internal/fat` writes the two files into the volume in Go, because neither tool copies files without mtools or a mount. It uses 8.3 names, a one-cluster directory each, and no third-party module.
3. **Every identifier is fixed**: the volume id and every timestamp from `SOURCE_DATE_EPOCH` in `image/build.env`, the disk and partition GUIDs from `image/disk/disk.env`.
4. **`make disk` also writes `naru.qcow2`**, the same disk converted with `qemu-img convert -O qcow2`, for Proxmox's `import_from`.
5. **The QEMU test boots `naru.img`** under OVMF with a read-only `OVMF_CODE_4M.fd` and a per-Node copy of `OVMF_VARS_4M.fd`, no `-kernel`, `-initrd` or `-append`, with the configuration and data disks attached.

## Consequences

- A Node boots from one disk on any UEFI host: Proxmox, QEMU with OVMF, or hardware.
- The command line is part of the kernel, so changing it is a kernel rebuild; `required.txt` pins it. A Node's address, which the direct-boot tests pass as `ip=`, comes from configuration, which is outside this ADR.
- The initramfs on the ESP is a file a reader can replace, and it is not signed; signing waits under `image.md` §Open questions.
- The ESP is at least 64 MiB, so a disk is larger than its contents (66 MiB for the P0 kernel and initramfs); the qcow2 is smaller where the unwritten clusters are not stored.
- A volume wants a writer of its own: `tooling/disk/internal/fat` is code naru maintains, checked in its tests against `fsck.fat`.
- The disk build needs `mkfs.fat`, `sgdisk` and `qemu-img` on the host, and the QEMU test needs OVMF.

## Signs the decision was wrong

- A firmware refuses the stub's `initrd=` load, or a Proxmox host boots the disk without the initramfs; the initramfs would then be embedded instead.
- The FAT writer is found to produce a volume a firmware or `fsck.fat` rejects, or grows past what a two-file volume needs; mtools or a vetted module would replace it.
- A tool of the disk build gives different bytes on another host, so `make image-repro` fails for the disk and not for the kernel.
- The built-in command line needs a per-Node value, so it is edited more often than the kernel is rebuilt.

## Errata

- 2026-11-15: paths moved in the image split ([ADR 0054](./0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)): `image/disk/disk.env` is `image/host/disk/disk.env` and `image/kernel/pin.env` is `image/common/kernel/pin.env`.
