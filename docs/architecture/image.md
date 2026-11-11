# Image

What the P0 image holds, where its inputs come from, and how a Node boots it.

Claims about garamsh/mvmd hold at commit `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64`; they promise nothing about later versions.

## Current decisions

- **P0 is done when a QEMU VM boots the image and init, as PID 1, starts and supervises the services the configuration names.** The services are mvmd, gaon's Node Agent and optionally gaon's control plane.
- **QEMU boots a kernel and an initramfs directly** (`-kernel`, `-initrd`). There is no installer and no disk layout for the system. The root is the read-only initramfs.
- **Two disks are attached.** The configuration disk is described in [config.md](./config.md). The data disk holds mvmd's state directory, formatted ext4 with the `verity` feature.
- **Host kernel.** kernel.org longterm 6.18.63, built by naru from the release tarball with naru's configuration, the version and the tarball's sha256 pinned in `image/kernel/`. It meets the conditions table of [ADR 0006](./adr/0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md).
- **Non-Go inputs** are `image/kernel/` (configuration and pin) and `image/initramfs/` (definition). Neither folder exists yet; the pull request of the issue that owns each creates it.
- **Binaries in the initramfs:**
  - mvmd and mvmd-supervisor, built with Go from a pinned mvmd commit, `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64` or later.
  - Firecracker, from the release that mvmd's `tooling/firecracker/fetch.sh` pins, verified by that pin.
  - gaon's binaries, built from a pinned gaon commit once gaon ships them. Until then the P0 test runs mvmd and a stand-in service, and gaon arrives by a pin bump.
  - Nothing is built or run in a container.
- **Not in the P0 image:** the guest kernel, and mvmd's guest-agent and converter initrds. They are needed only to run a Machine.
- **Forwarding is set by init**, not by mvmd ([config.md](./config.md)).

## Rationale

- [ADR 0004](./adr/0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md): why P0 boots a kernel and initramfs directly and keeps the state directory on a data disk.
- [ADR 0006](./adr/0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md): why the kernel is a pinned longterm tarball, which condition comes from which mvmd document, and why the binaries are built from pins.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): the obligations the image meets; the table of them is in [boundaries.md](./boundaries.md).

## Open questions

- **Installer.** How an image reaches a Node's disk is not decided.
- **A/B upgrade.** How a Node upgrades and rolls back is not decided.
- **Signing.** How an image, a kernel and a configuration are signed and verified is not decided.
- **Disk layout.** Partitions of an installed Node, and where the system lives other than as an initramfs, are not decided.
- **The guest artifacts.** Which guest kernel and which guest-agent and converter initrds a Node carries, and who builds the Rust initrds, is not decided; P0 omits them.
- **How the other mvmd obligations are met.** The cgroup placement of supervisors, the capabilities' grant and the nftables ownership are not decided; they wait under [boundaries.md](./boundaries.md) §Open questions.
