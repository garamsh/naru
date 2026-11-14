# ADR 0020: a Node boots one kernel file per slot of the ESP with the initramfs embedded, and a trial slot is reached once by BootNext

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#56. It supersedes item 1 of [ADR 0007](./0007-the-proxmox-boot-form-is-a-uefi-disk-image-whose-efi-stub-kernel-loads-the-initramfs-from-the-esp.md) (the initramfs is a file of the ESP that the stub loads) and item 2 of [ADR 0004](./0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md) (the initramfs is a separate artifact from the kernel), as far as the boot disk is concerned. The rest of both stands.

## Context

- **An upgrade needs a second place to put a kernel, and a way to try it once.** The system is an initramfs ([ADR 0004](./0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md)), so a new release is a new kernel and a new initramfs. If the new one does not come up, the Node must be back on the old one with no operator at the console.
- **The Linux EFI stub concatenates every `initrd=` file.** It reads each one named in the load options and in the built-in command line (`CONFIG_CMDLINE`) and joins them. With the initramfs on the ESP as `\INITRD.GZ` named in the built-in command line, a trial kernel with its own initramfs would be given both. An initramfs per slot is therefore not possible with a built-in `initrd=`, and a command line per slot would need a loader or a boot entry per slot that the disk cannot hold (below).
- **A disk image cannot hold boot entries.** UEFI keeps `Boot####`, `BootOrder` and `BootNext` in NVRAM, which a disk does not carry. A disk the firmware has never seen boots through the removable-media path, `\EFI\BOOT\BOOTX64.EFI`. Any other entry has to be created by something running on the Node.
- **Spike (garamsh/naru#56).** Under OVMF (the q35 machine with a persistent pflash vars file, as the QEMU tests run it), the running system wrote `Boot0100` for `\EFI\BOOT\TRIAL.EFI` through efivarfs and set `BootNext=0100`. The next boot started `Boot0100`, `BootNext` was gone on that boot, and the boot after started the default entry again. The test is `TestNode_BootNextBootsTheTrialSlotOnceAndTheNextRebootTheDefault`. BootNext is honoured, so systemd-boot's boot counting, which was the fallback if it was not, is not needed.
- **FAT has no atomic replace, but it has an atomic rename in one directory.** On vfat in kernel 6.18 `rename(2)` within one directory rewrites one directory entry. The rename is read from the vfat code and not tested under a power cut. Short (8.3) names with no long-file-name entries keep the rename a single entry rewrite.
- **The 2026-11-13 decision on the data disk binds**. The data image is test inputs only from v0.5.0, and #66 formats the separate scsi2 disk. Nothing a release needs may depend on a fresh data image.

## Decision

1. **A slot is one file of the ESP, a kernel with its initramfs embedded.** `CONFIG_INITRAMFS_SOURCE` names the cpio archive of `make image`, so `bzImage` is the kernel and the initramfs, and `image/kernel/embed.sh` makes it from `bzImage.bare` (the kernel before the initramfs) and `initramfs.cpio.gz`. The built-in command line is `console=ttyS0 panic=-1 loglevel=4` with no `initrd=`. `/dev/console` is a node in `image/initramfs/initramfs.list`, because an embedded archive replaces the kernel's default one, which supplied it.
2. **The ESP layout.** The disk is GPT, 1 MiB before the ESP and the ESP starting at sector 2048, with a fixed partition GUID (`Spec.ESPGUID`), FAT32, label `NARUESP`. The directory `\EFI\BOOT\` holds, with 8.3 names and no long names:
   - `BOOTX64.EFI`, the default slot. The firmware boots it when no entry is set.
   - `TRIAL.EFI`, the trial slot. A disk built by `tooling/disk` has none.
   The ESP is sized for two kernels, the larger of 64 MiB and two kernels with slack, rounded up to 1 MiB.
3. **The trial entry is created by the running system.** An upgrade writes `kernel.efi` of the bundle to `\EFI\BOOT\TRIAL.EFI`, then writes variable `Boot0100` (global variable GUID `8be4df61-93ca-11d2-aa0d-00e098032b8c`, attributes NV|BS|RT) and `BootNext=0100`. `Boot0100` is not in `BootOrder`, so only BootNext reaches it. The load option is attributes `1` (ACTIVE), the length of the device path, a description, and the device path `HD(1,GPT,<ESP GUID>,0x800,<ESP sectors>)/File(\EFI\BOOT\TRIAL.EFI)`, computable from the disk because the start sector and the GUID are fixed. The GUID is the raw bytes of the GPT entry. A variable in efivarfs is a file of four attribute bytes (`07 00 00 00`) and the value, and efivarfs marks a variable immutable, which the writer clears first. The kernel has `CONFIG_EFIVAR_FS=y`. Mounting efivarfs and the ESP, and writing the files, is the upgrade's work and not this decision's; the QEMU test does it with the test-only service `naru-bootnext`, which is not in the image.
4. **The health bound and the rollback trigger.** BootNext is used once: the firmware deletes it when it boots the entry. A trial boot is therefore a bound of one boot. If the trial kernel is not promoted before the next reboot, whatever the reason (it did not start, init did not reach health, the Node was reset), the next boot is the default slot, which was not touched. The trigger is a reboot, and the deciding code is the firmware's. What counts as healthy, and the promotion call, belong to the upgrade's implementation, not to this decision.
5. **Promotion is one rename.** `rename(TRIAL.EFI, BOOTX64.EFI)` within `\EFI\BOOT\`, which replaces the default slot by a single directory-entry rewrite, so a power loss leaves the old kernel or the new one and not a partial file (the rename is not tested under power loss). After it there is no trial slot, and the next upgrade starts from the state of a built disk. The old default kernel is gone after promotion; a rollback after promotion is another upgrade to the older bundle.
6. **The upgrade bundle is `naru-upgrade-<version>.tar`**, a tar with no timestamps and owner root, deterministic for the same inputs, holding two files in this order: `kernel.efi` (the `bzImage` of the release, the file that becomes a slot) and `manifest.json` (`version`, and `kernel` with `name`, `bytes` and `sha256`). The release lists it in `SHA256SUMS` and the workflow publishes it, so the release has nine files.
7. **Signing is out of scope.** The bundle's digest is in its manifest and in `SHA256SUMS`, which proves the bytes match what was published and nothing about who published them. Whether the firmware or the upgrade verifies a signature is not decided (image.md, open question).
8. **An existing single-slot disk needs one recreate.** A disk of v0.4.0 or earlier boots a kernel whose built-in command line has `initrd=/INITRD.GZ` and whose ESP has no room for a second kernel. It cannot be upgraded in place to a slot layout. Such a Node is recreated once from a release that carries this layout; every upgrade after that is an A/B upgrade. Nothing here changes a Node's data disk.
9. **No release depends on a fresh data image.** The data disk is test inputs, and the boot disk, the bundle and the upgrade work with the Node's own disks (the boot disk and the separate data disk #66 formats).

## Consequences

- **The boot disk has one form of kernel and no separate initramfs file.** `naru.img` holds `BOOTX64.EFI` and nothing else in `\EFI\BOOT\`; `\INITRD.GZ` is gone. A kernel and its initramfs cannot be paired wrongly, since they are one file.
- **The kernel depends on the initramfs.** `bzImage` is rebuilt whenever the initramfs changes (one more link step, `embed.sh`, with its own stamp), where it was built once. `bzImage.bare` is what the kernel build produces and caches.
- **The disk is larger by one kernel's worth** (about 33 MiB at present is the minimum free space for a second slot).
- **Reproducibility is still checked by `make image-repro`**, now on the embedded `bzImage` too.
- **The trial entry is not on the disk.** A Node that loses its NVRAM (a firmware reset) loses it, and boots the default. That is the safe direction.
- **Nothing here upgrades a Node.** The writer of `TRIAL.EFI`, the entry and the promotion, the health decision, and how a bundle reaches a Node are the upgrade's issues. The disk, the bundle and the boot behaviour they rely on are fixed here.

## Signs the decision was wrong

- A firmware under a hypervisor that Nodes run on (Proxmox with its OVMF) does not honour BootNext, or keeps a stale BootNext across a reset. The QEMU test shows OVMF with a persistent vars file only.
- A vfat rename within a directory is found not to be atomic on a power cut, so a Node cannot boot after promotion.
- The embedded initramfs's size makes a slot too large for the ESP the release can afford, or the extra link step makes `make image` too slow.
- A real need for a per-slot command line or an initrd that is not the system.

## Errata

- 2026-11-15: paths moved in the image split ([ADR 0054](./0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)): `image/initramfs/initramfs.list` is `image/host/initramfs/initramfs.list` and `image/kernel/embed.sh` is `image/host/kernel/embed.sh`.
