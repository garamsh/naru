# ADR 0003: naru deploys and supervises mvmd, builds both kernels, and carries mvmd's deployment constraints

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#2. The direction is the project owner's, recorded in garamsh/discussions Discussion #3 §결정 (decisions 5 and 6) and the reply [discussioncomment-19533418](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19533418). mvmd's constraints are read at garamsh/mvmd commit `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64` (`main` on 2026-11-11), under `docs/architecture/`; the files cited below are unchanged between that commit and `2288ea94810b8151abece78b47f2618db2a645d1`, the one the issue and Discussion #3 name, except `runtime.md` and `structure.md`, whose changes do not touch the passages cited. In this repository `Machine` names the microVM and `Node` the physical machine.

## Context

- **mvmd is the Node-local microVM runtime** and does not know gaon or naru. Its `structure.md` states "Nothing here states how an adapter is built inside, or how mvmd is deployed", so deployment is not decided on mvmd's side and its constraints are what it states elsewhere.
- **The guest kernel moves to naru.** The earlier decision ([discussioncomment-19466922](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19466922), 2026-11-08) put guest kernel build and management in a separate project. Discussion #3 decision 6 replaces "separate project" with "naru", and the reply of 2026-11-11 above restates it. The reasons of the earlier reply stand: Linux is GPLv2, so source has to be offered on distribution, and the build tool and update cadence differ from mvmd's. naru already carries both for the host kernel. The earlier reply stays as a record, and "mvmd holds no kernel configuration or patches" stands.
- **mvmd's own documents still name the earlier owner.** At the commit above, `guest.md` says the guest kernel project is created when a condition no published kernel meets becomes necessary and that a kernel meeting the conditions is supplied to the Node from outside mvmd. Both read against this ADR as a statement about a project that is now naru; mvmd's PM keeps that file.
- **Deployment constraints mvmd states**, each read at the commit above in the document named:
  - Supervisors outside mvmd's cgroup: `adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md`, Decision "Placement" and Consequences ("a deployment that does not loses VMs on every mvmd restart, though never past the expiry"); `runtime.md` §Fencing, "The supervisor runs outside mvmd's cgroup".
  - Privileges and kernel for the supervisor and for mvmd: the same ADR 0003, Consequences "Privileges and kernel" and Errata (2026-11-09); `adr/0015-the-supervisor-confines-every-vmm-process-the-same-way.md`, Consequences.
  - One nftables table pair kept across mvmd's restarts, and the kernel and capability it needs: `adr/0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md`, Consequences, and Errata (2026-11-09: two tables of the name `mvmd`, a netdev one and an inet one, replacing the single table the Decision text describes).
  - Forwarding, prefixes, gateway and resolvers per family: `adr/0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md`, Consequences.
  - fs-verity on the state directory's file system: `adr/0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md`, Decision "Where the cache lives" and Consequences.
  - Root disks as files in `disks/` of the state directory: `adr/0021-a-root-disk-is-a-file-in-the-disks-directory-of-mvmds-state-directory.md`, Decision and Signs the decision was wrong; `runtime.md` §State and stability, "Disks are files beside them".
  - `snapshots/` on the Machines' run directories' file system: `runtime.md` §State and stability, "Snapshots are files beside it".
  - The guest kernel's conditions: `guest.md` §Current decisions, "Kernel conditions" (read from ADR 0010 and ADR 0011 there).

## Decision

**naru deploys and supervises mvmd, builds the host kernel and the guest kernel, and meets mvmd's stated deployment constraints; mvmd keeps no kernel configuration or patches.**

1. **naru starts and supervises mvmd** as a configured service. The dependency is naru to mvmd by configuration only: naru runs what its configuration names.
2. **naru builds and manages the host kernel and the guest kernel.** mvmd holds no kernel configuration or patches.
3. **Every constraint below is an obligation on naru**, written as one a reviewer can look for in naru's image and configuration when they exist. A constraint is dropped here only by an ADR that supersedes this one, after mvmd's record changes.

   | Obligation | mvmd source |
   |---|---|
   | The service definition for mvmd puts every `mvmd-supervisor` process outside mvmd's cgroup, so stopping or restarting mvmd does not stop them. | ADR 0003 Decision "Placement" and Consequences; `runtime.md` §Fencing |
   | mvmd runs with `CAP_SYS_ADMIN` and `CAP_SYS_PTRACE`, and `CAP_NET_ADMIN`; the supervisor with `CAP_SYS_NICE` (or a nonzero `RLIMIT_RTPRIO` soft limit at or above its priority), `CAP_IPC_LOCK` (or an `RLIMIT_MEMLOCK` soft limit covering its memory), and until its child execs the VMM `CAP_SETUID`, `CAP_SETGID`, `CAP_SETPCAP`, `CAP_MKNOD` and `CAP_CHOWN`. | ADR 0003 Consequences and Errata; ADR 0015 Consequences; ADR 0012 Consequences |
   | The host kernel has `pidfd_open` (Linux 5.3), cgroup v2 with the memory, pids and cpu controllers, Landlock ABI 6 enabled (Linux 6.12 or later with `landlock` in the active LSM list), and a UID and GID range for VMMs; on a kernel built with `CONFIG_RT_GROUP_SCHED` the supervisors sit in the root cgroup. | ADR 0003 Consequences; ADR 0015 Consequences; `runtime.md` §VMM confinement |
   | The host kernel has `CONFIG_NF_TABLES` with the netdev and inet families, connection tracking and NAT. | ADR 0012 Consequences |
   | No other tool flushes the whole nftables ruleset or edits mvmd's tables, which are two tables of the name `mvmd` (netdev and inet); other firewall management stays out of them. A flush removes every Machine's filtering until mvmd's next start. | ADR 0012 Consequences and Errata |
   | IP forwarding is on for each family the Node serves, and the Node's configuration gives mvmd a prefix per family, an IPv4 gateway address if it serves IPv4, and resolvers. mvmd's documents at the commit above do not say whether mvmd sets forwarding itself; until they do, the host provides it. | ADR 0011 Consequences |
   | mvmd's state directory is on a file system with fs-verity enabled: ext4 with the `verity` feature, f2fs with it, or btrfs. XFS cannot host it, and mvmd refuses to start otherwise. | ADR 0008 Decision and Consequences |
   | `snapshots/` is on the same file system as the Machines' run directories. | `runtime.md` §State and stability |
   | Root disks stay files in `disks/` of the state directory. A deployment that needs them on another file system is a sign mvmd's decision was wrong; it is reported to mvmd and not worked around. | ADR 0021 Signs the decision was wrong |
   | The guest kernel meets mvmd's conditions: built in (the initrd carries no modules) `CONFIG_BLK_DEV_INITRD`, `CONFIG_VIRTIO_BLK`, `CONFIG_VIRTIO_VSOCKETS`, the virtio transport of the VMM, `CONFIG_EROFS_FS`, `CONFIG_OVERLAY_FS`, `CONFIG_EXT4_FS`, on x86_64 `CONFIG_PVH`, and what `guest.md` adds (among them `CONFIG_IPV6` for a Node that serves IPv6). | `guest.md` §Current decisions "Kernel conditions"; ADR 0010; ADR 0011 |

4. **None of these is decided by this ADR beyond naming it as naru's.** How an obligation is met (service manager, image layout, partitioning, kernel configuration) is not decided here; each waits under `docs/architecture/boundaries.md` §Open questions.

## Consequences

- naru's host kernel configuration, init's service definition for mvmd, the image's disk layout and the Node configuration are each constrained by a row above before any of them exists.
- A published guest kernel that mvmd currently pins (`guest.md` at the commit above) can stay in use until naru builds its own; this ADR does not change what mvmd boots.
- mvmd's constraints can change without naru's notice, so each is re-read at the mvmd commit current when naru's image or configuration is written, and a changed row is a new ADR.
- Where an obligation conflicts with a naru decision (a read-only image with the state directory on a file system without fs-verity, for instance), the conflict goes to mvmd's PM as a request, since mvmd's record is where the constraint can change.
- naru carries the guest kernel's GPLv2 source-offer duty beside the host kernel's.

## Signs the decision was wrong

- Obligations keep being added, so that naru's image is shaped around mvmd's deployment, not its own.
- A deployment needs a constraint broken: root disks on another file system, a file system without fs-verity for the state directory, or another firewall manager sharing nftables.
- mvmd states how it is deployed (its `structure.md` gains a deployment section), and the two records disagree.
- Guest kernel changes keep needing mvmd's configuration, so that mvmd ends up holding kernel configuration or patches.
- Changes to naru's host kernel and to the guest kernel are never made together, so one project owning both buys nothing (decision 6's stated reason).
