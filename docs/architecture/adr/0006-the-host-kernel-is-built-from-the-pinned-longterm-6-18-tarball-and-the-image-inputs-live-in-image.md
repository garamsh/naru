# ADR 0006: The host kernel is built from a pinned longterm 6.18 tarball, and the image's non-Go inputs live in `image/`

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#3, which is the record of the decision, and the PM decisions given to the worker in its brief. mvmd's conditions are read at garamsh/mvmd commit `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64`. The kernel release was read on 2026-11-11T03:10:39Z from `https://www.kernel.org/releases.json`, and its digest at 2026-11-11T03:10:48Z from `https://cdn.kernel.org/pub/linux/kernel/v6.x/sha256sums.asc`.

## Context

- **naru builds the host kernel** (Discussion #3 decision 6, [ADR 0003](./0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md)). P0 has to say from which source.
- **The newest longterm 6.18 release** on the date above is 6.18.63 (`releases.json` gives 2026-11-10). The sha256 of `linux-6.18.63.tar.xz` in `sha256sums.asc` is `81764a545072d160748beff4d9de2e7d95aa9f16e8261110288f067a2cc61d71`.
- **The kernel conditions.** Each row names where mvmd states it at the commit above. mvmd's `docs/architecture/` at that commit does not name `kvm_amd`, `kvm_intel` or the three virtio drivers: the modules are what `/dev/kvm` needs on the two CPU vendors, and the virtio drivers are what QEMU and Proxmox present to the Node. Those rows are naru's and carry no mvmd source.

  | Condition | Source |
  |---|---|
  | KVM, with `kvm_amd` and `kvm_intel`, so that `/dev/kvm` exists | mvmd `docs/architecture/adr/0015-the-supervisor-confines-every-vmm-process-the-same-way.md` and `docs/architecture/driver.md` name `/dev/kvm`; the two modules are naru's |
  | `CONFIG_NF_TABLES` with the netdev and inet families | mvmd `docs/architecture/adr/0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md` Consequences |
  | Connection tracking and NAT | the same ADR 0012 Consequences |
  | fs-verity on ext4 | mvmd `docs/architecture/adr/0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md` Decision "Where the cache lives" |
  | PID namespaces | mvmd `docs/architecture/adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md` Decision "Placement" and Consequences |
  | cgroup v2 with the memory, pids and cpu controllers | the same mvmd ADR 0003 Consequences |
  | Landlock in the active LSM list (ABI 6, Linux 6.12 or later) | mvmd ADR 0015 Consequences, and ADR 0003 of this repository |
  | virtio-blk, virtio-net and virtio-console, for QEMU and Proxmox | naru's; no mvmd source |

- **Where the non-Go inputs go.** [ADR 0001](./0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md) left it open. They are not Go modules and must stay outside its units.
- **The binaries.** At the commit above, mvmd's Go executables are `daemon/cmd/mvmd` and `daemon/cmd/mvmd-supervisor`, and its `tooling/firecracker/fetch.sh` pins Firecracker v1.17.0 with SHA-256 digests.

## Decision

**The host kernel is kernel.org longterm 6.18.y, built by naru from the release tarball with naru's own configuration, with the point release and the tarball's sha256 pinned; the non-Go build inputs live in one top-level folder, `image/`.**

1. **Source and pin.** Linux 6.18.63, `linux-6.18.63.tar.xz`, sha256 `81764a545072d160748beff4d9de2e7d95aa9f16e8261110288f067a2cc61d71`. The pin is in `image/kernel/` and moves by a pull request that changes the version and the digest together. Reasons: 6.18 is mvmd's guest series; Landlock ABI 6 needs Linux 6.12 or later; a longterm series receives fixes.
2. **Configuration.** naru's kernel configuration is in `image/kernel/` and satisfies every row of the table above.
3. **Layout.** `image/kernel/` holds the kernel configuration and pin, and `image/initramfs/` holds the initramfs definition.
4. **Binaries.** mvmd and mvmd-supervisor are built with Go from a pinned mvmd commit, `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64` or later. Firecracker comes from the release that mvmd's `tooling/firecracker/fetch.sh` pins, verified by that pin. gaon's binaries are built from a pinned gaon commit once gaon ships them; until then the P0 test runs mvmd and a stand-in service, and gaon arrives by a pin bump. No containers.
5. **Guest kernel and guest initrds.** The guest kernel, and mvmd's Rust guest-agent and converter initrds, are needed only to run a Machine. P0 ends when init starts and supervises services, so they are not in the P0 image.

## Consequences

- The kernel build is reproducible from a version and a digest, and a verified tarball is the only upstream input.
- naru carries the host kernel's GPLv2 source-offer duty ([ADR 0003](./0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md)) and a security-update cadence for the pin.
- `image/` is outside the Go workspace, so the Go checks do not cover it; how it is built and checked is for the issue that creates it.
- A Node cannot run a Machine from the P0 image until the guest artifacts are added.
- mvmd's conditions can change; each is re-read at the mvmd commit current when the configuration is written.

## Signs the decision was wrong

- A condition above needs a patch that no 6.18 release carries.
- The pin lags a published security fix for longer than naru can bear, so bumping it becomes continuous work.
- `image/` accumulates files that belong to a Go unit, or a unit starts needing files under it.
- The P0 test needs a Machine after all, so the guest artifacts must be in the image.

## Errata

- 2026-11-15: paths moved in the image split ([ADR 0054](./0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)): `image/initramfs/` is `image/host/initramfs/` and `image/kernel/` is `image/host/kernel/`, with the pin and `naru.config` in `image/common/kernel/`.
