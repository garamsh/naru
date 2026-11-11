# ADR 0005: Configuration is a YAML file on a labelled ext4 disk, in the shape of gaon's resources

> Status: superseded by [ADR 0008](./0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md) as to delivery
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#3, which is the record of the decision; the PM decisions of 2026-11-11 on delivery, file system and forwarding were given to the worker in its brief and are recorded here. The shape follows garamsh/gaon `docs/architecture/resource-api.md` at commit `d643b1cb6a6cc01ea587dd7c967bdfd07280fe0b`, §Common rules ("Every resource has `apiVersion`, `kind`, `metadata`, `spec` and `status`") and §Naming and groups (group suffix `gaon.garam.sh`).

## Context

- **P0 has no node API** ([ADR 0004](./0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md)), so init reads one file supplied at boot.
- **Two ways to hand a file to a VM were considered.** QEMU's `fw_cfg` is specific to QEMU. A disk is attached natively by QEMU and by Proxmox, the final target of the goal (garamsh/naru#6).
- **Which file system holds the file.** vfat was considered for writability without root tooling. The workstation has no mtools (`command -v mcopy mformat` printed nothing, checked by the PM on 2026-11-11) and bound 5 of garamsh/naru#6 forbids installing packages, so a vfat image cannot be populated there without root. `mkfs.ext4 -d <dir>` (e2fsprogs, `/sbin/mkfs.ext4`) builds a populated image without root, and the host kernel needs ext4 for the data disk anyway, so no second file system driver is added.
- **mvmd sets no forwarding.** The mvmd PM decided in garamsh/mvmd#311 that IP forwarding is a node prerequisite the host sets: mvmd never sets it and refuses to start when a served family has forwarding off. The fix that names naru as the guest kernel owner in mvmd's `guest.md` is garamsh/mvmd#305.

## Decision

**Init reads `naru.yaml` from a small ext4 disk labelled `NARUCFG`, mounted read-only; the file is a `NodeConfig` of `apiVersion: naru.garam.sh/v1alpha1` with `metadata` and `spec` and no `status`.**

1. **Delivery is a disk, not `fw_cfg`.** The disk holds one file, `naru.yaml`. Init finds the disk by file system label and mounts it read-only.
2. **The shape follows gaon's resources:** `apiVersion: naru.garam.sh/v1alpha1`, `kind: NodeConfig`, `metadata.name` the node name, and `spec`. There is no `status`, because a file has no observer.
3. **`spec` carries** the service list (name, binary path, args, env, and `controlPlane: bool` marking a service started only on the node that runs the control plane), `controlPlane: bool` for this node, the data disk (device or label, and mount point), and the network (forwarding families).
4. **Init turns on `net.ipv4.ip_forward` and `net.ipv6.conf.all.forwarding`** for the families the configuration names, before it starts mvmd. This meets the forwarding obligation of [ADR 0003](./0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md), per garamsh/mvmd#311.

## Consequences

- Any host that can attach a disk can configure a Node, and a disk image is built without root by `mkfs.ext4 -d`.
- The configuration is a boundary input: init rejects an unknown `apiVersion` or `kind`, and a file that does not parse, before starting any service.
- The same kind can later be accepted by the node API, since both carry the same shape. The API's shape is not decided here.
- The configuration is not confidential in P0: whoever holds the disk can read it.
- Fields beyond P0 (the prefix per family, IPv4 gateway and resolvers that mvmd's Node configuration needs, host network settings for connecting Nodes) are not decided and wait under `config.md` §Open questions.

## Signs the decision was wrong

- A target cannot attach a second disk at first boot.
- Hosts keep failing to build the ext4 image without root.
- Operators keep needing fields that depend on observed state, so a `status` is wanted.
- naru's group name `naru.garam.sh` collides with a name gaon or garam settles for naru's resources.
