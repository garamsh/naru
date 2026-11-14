# ADR 0010: The guest kernel and initrds are image components, built at the pinned mvmd commit

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#26, part of goal garamsh/naru#25. It supersedes item 5 of [ADR 0006](./0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md) (the guest kernel and initrds are not in the image); the rest of that ADR stands. mvmd's conditions are read at garamsh/mvmd commit `3b37892f303a4d4dbd8e5360906d0f9c036b562e`, `docs/architecture/guest.md` §Kernel conditions and its ADRs 0010, 0011, 0016 and 0041.

## Context

- **A Node cannot start a Machine without a guest kernel and two initrds.** mvmd's Driver config names the kernel and each initrd by path and SHA-256 (`kernel`, `kernel_sha256`, `initrd`, `initrd_sha256`, for the Firecracker driver and for the converter). Until now the image carried none of them: the QEMU tests put mvmd's `fetch.sh` download (Kata Containers' 6.18.56 kernel) and the two initrds on the data disk, and a release's Nodes ran on test inputs.
- **mvmd's conditions on the guest kernel** (guest.md §Kernel conditions at the commit above) are: `BLK_DEV_INITRD`, `VIRTIO_BLK`, `VIRTIO_VSOCKETS` with a virtio transport, `EROFS_FS`, `OVERLAY_FS`, `EXT4_FS` and an uncompressed ELF with a PVH entry (`PVH`); for the Guest Agent `DEVTMPFS`, `PROC_FS`, `PROC_SYSCTL`, `SYSFS`, `PCI`, `VIRTIO_PCI`, `HOTPLUG_CPU`, `NET`, `INET` and `VIRTIO_NET`; for the kernel's own reseed after a restore `VMGENID` and `ACPI`; and `IPV6`. Firecracker runs with `--enable-pci` and every non-root disk is hot-plugged at Start (mvmd ADR 0041), so the guest needs PCI hot-plug as well.
- **The host kernel's source is already pinned.** [ADR 0006](./0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md) pins `linux-6.18.63.tar.xz` by SHA-256 and checks the final `.config` against `required.txt`.
- **The image is an initramfs that init copies to a tmpfs root.** Everything in it costs memory for as long as the Node runs ([init.md](../init.md), `linux.LeaveRootfs`), so what is added must be measured.
- **mvmd will take several kernels.** garamsh/mvmd#364 (not implemented at the commit above) makes the Driver config take `kernels`, a map from a name to `{path, sha256}`, and `default_kernel`; a name is 1 to 63 characters of `[a-z0-9-]`.

## Decision

**The guest kernel, the Guest Agent's initrd and the converter's initrd are built by naru at the pinned mvmd commit and are files of the image, at fixed paths, named in a `guest.json` that the build writes and the image carries.**

1. **The guest kernel is built from the host kernel's tarball.** `image/guest/kernel.sh <name>` unpacks the tarball that `image/kernel/` pins (the same version and SHA-256, checked again), merges `image/guest/kernels/<name>/naru.config` over x86_64 `tinyconfig`, runs `olddefconfig`, fails unless every line of `image/guest/kernels/<name>/required.txt` matches a line of the final `.config` (as the host kernel's check does, since kconfig drops an unmet option without failing), and builds the uncompressed ELF `vmlinux`. Every driver is built in, `CONFIG_MODULES` is off, and the initrd carries no modules. The configuration is naru's: `tinyconfig` starts from nothing, so each option is a line that someone asked for. `required.txt` has a line for each condition of guest.md §Kernel conditions listed above, including `CONFIG_VMGENID=y` and `CONFIG_ACPI=y`, and one for `# CONFIG_MODULES is not set`.
2. **The initrds are built by mvmd's Makefile** (`make guest-initrd converter-initrd`) in a checkout of the pinned commit, as before, and `image/guest/pin.env` holds their SHA-256; mvmd builds each twice and fails unless the two are equal. At `3b37892` the digests are the ones of the previous pin (`34e7b34a…` and `06b6a7a9…`).
3. **The image holds them under `/usr/lib/naru/guest/`:** `kernels/<name>/vmlinux`, `guest-initrd.cpio`, `converter-initrd.cpio` and `guest.json`, mode 0644, owned by 0:0. `guest.json` lists the kernels as a `kernels` map from name to `{path, sha256}`, a `default_kernel`, and `guest_initrd` and `converter_initrd` as `{path, sha256}`; the paths are the ones inside the image and the SHA-256 are read from the files the build wrote.
4. **The kernels are declared in `image/guest/kernels.env`:** `GUEST_KERNELS` (a list of names, each a directory of `image/guest/kernels/`) and `GUEST_DEFAULT_KERNEL`. One kernel is declared, **`naru`**. Each name is checked against `[a-z0-9-]{1,63}` by `kernel.sh` and again by the Go reader of `guest.json`.
5. **The configuration is made from `guest.json`.** The QEMU tests and the release stage nothing of the guest: `tooling/qemu/internal/guest` reads `guest.json` from the build output, and `node.Config.MvmdJSON` writes `firecracker.kernel`, `kernel_sha256`, `initrd`, `initrd_sha256` and the converter's from it, with the kernel taken from the default. A path or a digest is written once, by the build.
6. **The data disk no longer carries the kernel or the initrds.** It holds the test-only programs, the registry and the mke2fs staging.
7. **The release publishes `naru-guest-<version>.json`,** a copy of `guest.json`, and lists it in `SHA256SUMS`, so the names and digests of the guest files are readable without unpacking the initramfs.
8. **`image/mvmd-checkout.sh`** fetches the pinned commit once for both `image/initramfs/build.sh` and `image/guest/build.sh`.

## The mapping to mvmd#364

The declared list is already in the shape mvmd#364 takes, with no renaming: `guest.json`'s `kernels` is the `kernels` map and `default_kernel` is `default_kernel`, each entry `{path, sha256}`, each name within `[a-z0-9-]{1,63}`. When naru's pin moves past that change, `MvmdJSON` writes `kernels` and `default_kernel` from `guest.json` unchanged and stops writing the single `kernel` and `kernel_sha256`. Until then, at `3b37892`, it writes the single fields from the default kernel. A second kernel is a new directory under `image/guest/kernels/` and a name in `GUEST_KERNELS`; nothing else moves.

## Added size and memory

Measured at this commit, against the same tree before the guest files were added (both at mvmd `3b37892`):

| | Before | After | Added |
|---|---|---|---|
| `initramfs.cpio` (uncompressed) | 36,894,208 B | 58,129,920 B | 21,235,712 B (20.3 MiB) |
| `initramfs.cpio.gz` (what the ESP holds) | 14,395,344 B | 20,411,586 B | 6,016,242 B (5.7 MiB) |

The files are the 16,166,832 B `vmlinux`, the 2,970,928 B guest initrd and the 2,096,180 B converter initrd. init copies the extracted initramfs to a tmpfs and removes each entry from the rootfs as it is copied, so a Node holds the extracted size in memory for as long as it runs, about 20.3 MiB more than before, on top of the compressed file that the kernel frees after unpacking it. The resident figure was derived from the file sizes and init's code, not read from a running Node. The boot disk's ESP grows by the compressed 5.7 MiB.

## Consequences

- A release's Nodes can start a Machine with nothing added to them: the kernel and the initrds are the image's and their digests are in a published asset.
- The guest kernel is naru's configuration, not Kata's; a Machine's boot is tested on it (`TestNode_MachineSurvivesMvmdRestart`).
- Every Node carries about 20.3 MiB more memory in its root for the guest files, and the image is about 5.7 MiB larger.
- The image's bytes now depend on a second kernel build and on mvmd's initrd builds; `make image-repro` covers both (the guest manifest holds the SHA-256 of every guest file).
- mvmd's `fetch.sh` still downloads Kata's kernel when it fetches Firecracker; naru keeps only the Firecracker lines of its `inputs.env`. That download is wasted; avoiding it needs a change in mvmd.
- Changing a guest kernel option is a change to a `naru.config` and its `required.txt` in a pull request; the version moves with the host kernel's pin.

## Signs the decision was wrong

- The memory of the guest files in the root is a problem on a Node (small RAM, or Machines that need it), which would move them to a disk read through a mount rather than the tmpfs.
- A Machine fails to boot on the guest kernel for an option `required.txt` does not list; the missing option is then added to both files.
- A second kernel is wanted that does not come from the same tarball; the tarball rationale then splits per kernel.
- mvmd#364 changes shape before naru's pin reaches it, so that `guest.json` no longer maps to the Driver config without renaming.

## Errata

- 2026-11-15: paths moved in the image split ([ADR 0054](./0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)): `image/guest/` is `image/host/guest/` (`build.sh`, `kernels/`, `kernels.env`, `pin.env`), `image/guest/kernel.sh` is `image/common/kernel/kernel.sh`, `image/initramfs/build.sh` is `image/host/initramfs/build.sh`, `image/kernel/` is `image/common/kernel/` and `image/host/kernel/`, and `image/mvmd-checkout.sh` is `image/host/mvmd/checkout.sh`.
