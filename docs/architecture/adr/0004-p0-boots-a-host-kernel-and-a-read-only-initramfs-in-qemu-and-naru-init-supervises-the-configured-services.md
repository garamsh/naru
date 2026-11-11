# ADR 0004: P0 boots a host kernel and a read-only initramfs in QEMU, and naru's init supervises the configured services

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#3, which is the record of the decision. The direction is the project owner's, stated in the naru PM session of 2026-11-11 and recorded in that issue, under garamsh/discussions Discussion #3 decision 5. In this repository `Machine` names the microVM and `Node` the physical machine.

## Context

- **naru's first milestone needs a boundary.** gaon's end-to-end test (garamsh/gaon#9) waits on a Node that boots and runs services, so P0 is limited to that.
- **The node API, its PKI, upgrade, reset, the installer and PXE are not decided** (Discussion #3 §제안, phases P1 and P2), so P0 cannot depend on any of them.
- **mvmd's state directory needs fs-verity** (garamsh/mvmd `docs/architecture/adr/0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md`, Decision "Where the cache lives"; the state-directory row of [ADR 0003](./0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md)), so it cannot sit on a read-only initramfs.

## Decision

**P0 is done when a QEMU VM boots naru's host kernel and initramfs directly, and naru's init, as PID 1, starts and supervises the services its configuration names.**

1. **The services** are mvmd, gaon's Node Agent (`gaon-node-agent`), and optionally gaon's control plane (gaon-apiserver, gaon-scheduler, gaon-controllers, a single-member etcd) on one node. naru knows each service's name, binary and arguments from configuration and nothing of gaon's or mvmd's internals.
2. **QEMU boots a host kernel and an initramfs directly** (`-kernel`, `-initrd`). There is no installer and no disk layout for the system. The root is the read-only initramfs.
3. **mvmd's state directory is on a separate data disk** formatted ext4 with the `verity` feature.
4. **Out of P0:** the node API and its PKI, upgrade, reset, the installer, signing, and building the guest kernel.

## Consequences

- P0 is testable without any other naru unit: init, the image and a configuration are enough.
- The root being read-only means everything mutable lives on the data disk or in a tmpfs, and the initramfs holds every binary of P0.
- A host that boots a different way (Proxmox attaching disks, PXE) is not covered; [ADR 0005](./0005-configuration-is-a-yaml-file-on-a-labelled-ext4-disk-in-the-shape-of-gaons-resources.md) keeps configuration independent of the boot form.
- Disk layout, upgrade and signing wait under `image.md` §Open questions.

## Signs the decision was wrong

- The initramfs outgrows memory that the target Nodes can spare, once gaon's binaries are in it.
- A target needs a persistent root, so the read-only initramfs forces state onto the data disk that does not belong there.
- gaon's test needs a service that P0 omitted before P1 exists.
