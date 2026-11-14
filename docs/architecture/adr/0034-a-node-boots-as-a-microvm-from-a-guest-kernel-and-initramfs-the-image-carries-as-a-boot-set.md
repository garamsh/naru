# ADR 0034: a Node boots as a microVM from a guest kernel and initramfs the image carries as a boot set

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#98. It builds on [ADR 0005](./0005-configuration-is-a-yaml-file-on-a-labelled-ext4-disk-in-the-shape-of-gaons-resources.md)'s labelled ext4 data disk, [ADR 0010](./0010-the-guest-kernel-and-initrds-are-image-components-built-at-the-pinned-mvmd-commit.md)'s guest kernel and initrds and [ADR 0020](./0020-a-node-boots-one-kernel-file-per-slot-of-the-esp-with-the-initramfs-embedded-and-a-trial-slot-is-reached-once-by-bootnext.md)'s embedded initramfs by reference; no body of them is edited. This is part A of #98, the guest image and boot; the host API's OS form is ADR 0035, part B, written in its own pull request.

## Context

- **mvmd boots a guest from an OS form.** mvmd ADRs 0058 to 0061 give a Create Machine `os_boot = {boot_set, kernel_cmdline}`. A boot set is named in mvmd's driver configuration as a kernel and an initrd, each with a path and a SHA-256. The Driver's parameters come first on the command line. Firecracker stops a guest by Ctrl-Alt-Del through the i8042 and the AT keyboard, and Cloud Hypervisor through the ACPI power button.
- **The image had no guest kernel for it.** `image/guest/` built the Guest Agent's and the converter's kernels, from `tinyconfig`, for mvmd's own guests. A Node that is itself a guest needs the kernel a host Node runs, and an initramfs that holds init, containerd, runc, the Kubernetes binaries and the rest, without the host-only services.
- **A blank data disk had no one to format it.** A host Node's data disk is made by the disk image build. A guest Node's is a Create Disk of at most 1 MiB, which cannot hold a file system.
- **Ctrl-Alt-Del restarted the kernel.** Linux reboots at once on it unless init takes it as a signal.

## Decision

1. **The image carries one boot set, `naru-node`.** The guest build writes `guest/boot-sets/naru-node/vmlinux`, and the initramfs build writes `guest/boot-sets/naru-node/initrd.cpio.gz`, both under `/usr/lib/naru/guest/`, with the same ownership and mode as the rest of that directory.
2. **The kernel is the host kernel's configuration with a small overlay.** `image/guest/kernel.sh` reads `kernels/<name>/base`: `tinyconfig`, the default, or `host`. The `node` kernel is `host`: `defconfig`, `image/kernel/naru.config`, then `image/guest/kernels/node/naru.config`, which sets PVH entry, `VIRT_DRIVERS` and VMGENID, virtio-mmio, the i8042 and AT keyboard, the ACPI button and WireGuard, and unsets EFI, efivarfs and the built-in command line. `required.txt` lists what the final `.config` must hold, and the build fails when it does not. The existing `naru` kernel is unchanged.
3. **The initrd is the host initramfs minus the host-only files.** `image/initramfs/guest-excludes.txt` lists the `file` entries of `initramfs.list` the guest does not carry (`mvmd`, `mvmd-supervisor`, `firecracker`, the four Gaon binaries and `naru-hostd`); the build fails if an excluded path has no entry. What is left is archived with `gen_init_cpio` and compressed with `gzip -9n`, as the host archive is. `initramfs.list` is not split, because the QEMU tests and the release code read it expecting every binary.
4. **The boot set is named in `boot-sets.json`, beside `guest.json`, not in it.** The file holds mvmd's `boot_sets` shape with the SHA-256 of each file, so mvmd's driver configuration can take it as it is. `guest.json` is read by strict parsers in `naructl/internal/layout` and `tooling/qemu/internal/guest`, which refuse an unknown field and are outside this part. Folding the boot set into `guest.json` is left to the part that touches those parsers.
5. **init formats a blank data disk only when `spec.dataDisk.formatBlank` is `true`.** With it, and when no ext4 disk carries the configured label, init formats the one whole disk whose first MiB is all zeros as ext4 with the verity feature, a 4096-byte block and the label, using the image's `mke2fs`, and logs it. A disk that holds anything is never formatted, a disk already labelled is left alone, and more than one blank disk is an error: init does not choose. `formatBlank` needs `spec.dataDisk.label`. Without the field nothing changes.
6. **init takes Ctrl-Alt-Del as SIGINT.** After it subscribes to signals, init calls `reboot(LINUX_REBOOT_CMD_CAD_OFF)`, so the stop request reaches the one stop path, which stops the services and then reboots. A failure is logged and boot goes on.
7. **The mvmd pin is the one on `main`, `191326aca748a0e6e365578016ab87455107164e`,** which is `d19cecb1726835871342af6d91a092c448f1f1a7`, the commit that carries the OS form, and one commit after it. This part does not move the pin. The claims above about mvmd's OS form were read at `d19cecb`, not read again at `191326a`. The Guest Agent initrd's digest in `image/guest/pin.env` is the one `main` holds; the build confirms it or this part changes it.
8. **`make image-repro` compares the new files** (`node-initrd.cpio.gz`, `boot-sets.json` and the `naru-node` kernel) as it does the others.

## Consequences

- **Sizes at this commit.** The `naru-node` kernel is 58,244,384 bytes, the node initrd 155,266,130 bytes, and the host initramfs 368,800,279 bytes with both inside it. The host image's `bzImage` is 384,558,080 bytes.
- **The host initramfs carries a second copy of init and the Kubernetes binaries,** compressed inside the first. A host Node holds the boot set in memory until the root is no longer needed; the cost is memory, not disk.
- **The criterion that `guest.json` lists the boot set is not met here.** `boot-sets.json` does, and the later part folds it in.
- **Kubernetes in the guest is untested here.** Part C boots the guest in QEMU; this part builds and checks the files and the init behaviour by unit test.
- **The `formatBlank` path is exercised by unit test and a mutation check,** not by a boot. The Ctrl-Alt-Del change has no unit test: it is one system call that needs PID 1.
- **A pin move needs the kernel and initrd digests moved with it,** as before.

## Alternatives rejected

- **A `tinyconfig` kernel for the Node.** It would need every option a host Node needs, listed again and kept in step with `image/kernel/naru.config`. The `host` base shares them.
- **Splitting `initramfs.list` into a common and a host-only file.** The tests and the release code read the one file; a list of exclusions leaves them as they are.
- **Putting the boot set in `guest.json` now.** It changes two strict parsers outside this part's paths.
- **Formatting a blank disk without being asked.** A disk that is blank because it failed to attach would be formatted as a data disk; the field makes it the Node's configuration, which `naructl` sets for a guest Node only.
