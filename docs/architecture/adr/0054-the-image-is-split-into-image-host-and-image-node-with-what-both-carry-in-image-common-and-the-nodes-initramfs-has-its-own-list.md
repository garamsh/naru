# ADR 0054: the image is split into image/host and image/node, with what both carry in image/common, and the Node's initramfs has its own list

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#116 (plan: garamsh/naru#113). It reorganizes the inputs [ADR 0006](./0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md) put under `image/` and supersedes item 3 of [ADR 0034](./0034-a-node-boots-as-a-microvm-from-a-guest-kernel-and-initramfs-the-image-carries-as-a-boot-set.md).

## Context

- **One folder per tool, not per image.** `image/` held one folder for each program or kernel the build makes, and the host image and the Node image were told apart only by a list of exclusions: `image/initramfs/guest-excludes.txt` named the host-only files, and the build derived the Node's list from the host's. A reader could not tell from the tree what the Node carries, and a file added to the host list went into the Node's initrd unless someone remembered the exclusions.
- **Two images build from the same inputs.** Both carry init, `mkfs.ext4`, `mkfs.xfs`, `nft`, `mount`, `umount`, the credential helpers and the root bundle, and both kernels come from one tarball and one base configuration.
- **The host image carries the Node's runtime.** The boot disk's own Node form ([ADR 0007](./0007-the-proxmox-boot-form-is-a-uefi-disk-image-whose-efi-stub-kernel-loads-the-initramfs-from-the-esp.md), [ADR 0031](./0031-a-node-runs-kubernetes-as-init-services-containerd-and-runc-are-static-and-pods-get-routed-prefixes-from-one-pod-network.md)) runs Kubernetes, containerd, runc, the CNI plugins and etcd from the host initramfs, which is what `TestNode_` and `TestControlPlane_` boot.

## Decision

1. **`image/` has three folders.** `image/host/` holds what only the host image builds (the host kernel's script, `embed.sh` and `required.txt`; the host initramfs; the disk; the mvmd pin and its checkout script, `image/host/mvmd/checkout.sh`, formerly `image/mvmd-checkout.sh`; the guest kernels and initrds; gaon). `image/node/` holds what only the Node image builds (its kernel overlay, `required.txt` and script; its initramfs; `boot-set.env`, the boot set's name `naru-node`; Kubernetes, containerd, the CNI plugins and etcd). `image/common/` holds what both build. `image/build.env` stays directly under `image/`, because every script in all three sources it.
2. **Each component of `image/common/` is there for a reason.**
   - `cacert`, `ecr-login` and `rolesanywhere`: both initramfs carry the bundle and the two helpers, for registry credentials ([ADR 0026](./0026-a-node-takes-registry-credentials-and-trust-as-configuration-an-ecr-credential-comes-from-iam-roles-anywhere-and-an-agent-pair-without-the-required-organization-is-set-aside.md)).
   - `e2fsprogs` and `xfsprogs`: init formats a blank data disk and a storage disk in either image ([ADR 0011](./0011-mkfs-ext4-is-a-static-mke2fs-built-from-the-pinned-e2fsprogs-release-and-installed-in-the-image.md), [ADR 0027](./0027-a-node-offers-an-extra-disk-as-xfs-storage-for-mvmds-disks-formatted-when-blank-mounted-hardened-and-grown-at-boot.md)).
   - `nftables` and `util-linux`: `nft`, `mount` and `umount` are in both lists.
   - `kernel`: `pin.env` (the kernel tarball) and `naru.config` are the host kernel's configuration, which the Node's kernel is built over; `kernel.sh` builds a kernel from a directory holding a `naru.config`, a `required.txt` and optionally a `base` file, for the guest kernels and the Node's. Its argument is the kernel's directory, and the kernel's name is that directory's name.
   - `initramfs/lib.sh`: `gobuild`, `require_static` and `build_gen_init_cpio`, which the two initramfs builds share.
3. **The Node's initramfs has its own list, `image/node/initramfs/initramfs.list`.** It is no longer the host's list less `guest-excludes.txt`, which is deleted; this supersedes ADR 0034 item 3. The first list was generated from the old derivation, so the archive's bytes did not change by the split.
4. **The build order is the Node's kernel, the Node's initramfs, then the host initramfs.** `image/node/kernel/build.sh` writes `guest/boot-sets/naru-node/vmlinux`; `image/node/initramfs/build.sh` writes `node-initrd.cpio.gz`; `image/host/initramfs/build.sh` fails unless that file exists, then writes `boot-sets.json` and embeds the initrd, the kernel and the file in the host initramfs. `image/host/guest/build.sh` no longer builds the Node's kernel.
5. **The host image keeps the Node's runtime** because the boot disk's Node form runs from it (Context). `image/host/initramfs/initramfs.list` names Kubernetes, containerd, runc, the CNI plugins and etcd from `image/node/`.
6. **Outputs do not move.** `build/image/...` and the release asset names are as before, so no consumer outside `image/` changes but the path strings that read the inputs (`tooling/qemu`, `lib/init`, `naructl`, `tooling/disk`, the release workflow).

## Rejected

- **A manifest of shared components beside the old tree.** It would name the split without making the tree show it.
- **`image/common/` holding every component.** The Node image's inputs and the host image's would again be told apart only by the lists.
- **Moving `build.env` into `image/common/`.** The root file fixes the environment of all three folders; it would be a common file only by name.

## Accepted risks

- **Two initramfs lists can drift.** A program both carry is listed twice. The reproducibility check builds both and a QEMU test boots each.

## Consequences

- **What would remove the Node runtime from the host image.** Giving the boot disk's Node form up, or serving it from a boot set as the guest form is, would let the host list drop Kubernetes, containerd, runc, the CNI plugins and etcd. Neither is decided here.
- **Moving `naru-orchd` and `naru-signer` out of the host initramfs** is a later change; the Node's list does not carry them.
- **Paths that name the old tree** in the accepted ADRs are corrected by errata lines, and the responsibility documents `image.md`, `structure.md` and `init.md` name the new ones.
- **The PM's final-check script** reads `image/host/mvmd/checkout.sh` where it read `image/mvmd-checkout.sh`.

## Signs the decision was wrong

- A change to one image repeatedly needs an edit under the other image's folder.
- A file of `image/common/` is used by only one image for a release.
