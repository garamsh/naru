# ADR 0027: a Node offers an extra disk as XFS storage for mvmd's disks, formatted when blank, mounted hardened and grown at boot

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#66, part of garamsh/naru#62 (E5) and garamsh/aurora#749. It amends [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) by reference: its body is not edited, and what it says about the classes and the keyed lists holds with this decision added. It follows [ADR 0011](./0011-mkfs-ext4-is-a-static-mke2fs-built-from-the-pinned-e2fsprogs-release-and-installed-in-the-image.md) for how a formatter reaches the image, and [ADR 0008](./0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md) for how the configuration reaches the Node.

## Context

- **A Node's Machines keep their disks on the data disk.** mvmd's disks are files under `<state_dir>/disks`, and naru's `state_dir` is `/var/lib/naru/mvmd` on the data disk, an ext4 with `verity` of a fixed small size ([ADR 0003](./0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md)). A Node that runs many Machines needs a larger disk of its own for them, and one whose file system clones a file in constant time, since mvmd derives a Machine's disk from an image by copying.
- **mvmd at `d189133bba8d842d2bcb20046f39fe03161efeb1` has no disk-directory setting.** It hard-codes `<state_dir>/disks`, which is `/var/lib/naru/mvmd/disks` here. A disk that is to hold mvmd's disks has to be what is mounted at that path.
- **init can only mount what it can find.** The image has no udev, so `/dev/disk/by-id` does not exist, and no `blkid`. Init already finds the data disk and the NoCloud drive by reading superblocks ([init.md](../init.md)). A formatter is a program, and the image's programs are static, because it has no libc ([ADR 0011](./0011-mkfs-ext4-is-a-static-mke2fs-built-from-the-pinned-e2fsprogs-release-and-installed-in-the-image.md)).
- **A disk that init formats is data lost when init is wrong.** The first boot of a Node with a blank disk formats it; every later boot must find the same disk and leave it alone, and a disk that holds something else must never be touched.
- **The kernel has no modules.** What a Node's disks need must be built in, and a kernel option reaches a running Node only with an upgrade bundle ([ADR 0020](./0020-a-node-boots-one-kernel-file-per-slot-of-the-esp-with-the-initramfs-embedded-and-a-trial-slot-is-reached-once-by-bootnext.md), [ADR 0025](./0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md)).

## Decision

1. **`spec.storage`** is a list of `{name, label | device, form, mountPoint}`, keyed by `name`. `name` is a DNS label of at most 12 characters, since it becomes the file system label, and no `name` is listed twice. Exactly one of `label` and `device` is given: `label` is a GPT partition label (at most 36 bytes) or the label of an XFS file system, and `device` is an absolute path such as `/dev/sdb`. `form` is `xfs`, which is also the default, and nothing else is accepted. `mountPoint` is optional, an absolute clean path, and `/var/lib/naru/storage/<name>` when absent; no two entries, and not the data disk, share a mount point. A configuration that breaks one of these is refused, naming `spec.storage[i]` and the field.
2. **`device` is a path of the Node's `/dev`, not a `/dev/disk/by-id/…` one.** The image has no udev, so no such path exists on a Node. `label` finds a partition by its GPT name, or an XFS disk by its label; a blank whole disk has neither, and is found by `device`. An ext4 label is not matched.
3. **`spec.storage` is reboot-class.** A change is refused in `no-reboot` mode and taken at the next boot, as ADR 0018 provides for the fields that are written once at boot. It is a keyed list, keyed by `name`: a merge patch names an entry by `name` and `$patch: delete` removes it.
4. **Init prepares each disk at boot, after it has written `spec.files` and before it sets the clock, issues certificates or starts any service.** The data disk is mounted by then, and the root is still writable, so the mount point can be made. For each entry, in order:
   - Init finds the disk, waiting up to 10 s as for the data disk. A disk that is not there is logged at error level, and the boot goes on.
   - If the mount point already holds files, init mounts nothing over them, logs an error naming how many entries it holds, formats nothing, and the boot goes on.
   - If the disk holds an XFS file system, init does not format it, whatever its label.
   - If the first 1 MiB of the disk is all zero, init formats it as `mkfs.xfs -m reflink=1,crc=1 -L <name>` and logs `storage disk formatted as XFS`.
   - If its first MiB holds anything else, another file system included, init logs an error, leaves the disk untouched, and the boot goes on.
   - Init mounts it at the mount point with `nodev,nosuid,noexec`, makes the mount point mode `0700`, and logs `storage disk mounted`.
   A failure to prepare a disk is therefore not a refusal of the boot, as a route the kernel refuses is not ([ADR 0023](./0023-spec-network-routes-are-static-ipv4-routes-init-applies-live.md)): a Node whose extra disk is gone still runs its other services, and the log says what is missing.
5. **Init grows the file system at the boot that follows the disk's growth.** After mounting, init compares the device's size with the file system's, and when the device is larger it issues `XFS_IOC_FSGROWFSDATA` on the mount point for the whole device, and logs `storage file system grown`. The image carries no `xfs_growfs`. Growth without a reboot is not provided: a disk enlarged under a running Node is used at its next boot. Shrinking is not offered, and a disk that is now smaller than its file system is mounted as it is.
6. **mvmd's disks are on the storage disk by mounting it over `/var/lib/naru/mvmd/disks`.** `naructl gen node --storage NAME=VALUE`, repeatable, writes `spec.storage` with `device: VALUE` when VALUE starts with `/dev/` (a blank disk has no label, so this is the form a first use takes) and `label: VALUE` otherwise, and, for the first of them, `mountPoint: /var/lib/naru/mvmd/disks`; the others take the default mount point. The data disk's own `mvmd/disks` directory is what the mount hides, so there is no migration: a Node that already has files in it keeps them there, init does not mount over them (item 4), and moving them is the operator's. This is the in-place path's rule. It rests on mvmd's hard-coded directory, and it is dropped when mvmd takes the directory as a setting.
7. **The kernel gains** `XFS_FS`, `BLK_DEV_DM`, `DM_THIN_PROVISIONING`, `DM_SNAPSHOT`, `BLK_DEV_LOOP` and `BLK_DEV_NBD`, built in, with the options they select (`DM_BUFIO`, `DM_PERSISTENT_DATA`, `DM_BIO_PRISON`, `FS_IOMAP`) listed in `required.txt` so that a change that loses one fails the kernel build. `XFS_ONLINE_SCRUB` is off, because the image has no `xfs_scrub` to use it. The device mapper, loop and NBD options are for mvmd's disk backends, which a Node's Machines will use; init uses none of them.
8. **`mkfs.xfs` is a static binary built from pinned sources, installed as `/bin/mkfs.xfs`.** `image/xfsprogs/` pins xfsprogs, util-linux (for libuuid and libblkid), userspace-rcu and inih, each by URL and SHA-256, and its build script builds the libraries into the build directory, builds `mkfs.xfs` only, strips it and fails unless `file` reads it as statically linked. The build is reproducible and `make image-repro` compares it with the other outputs. A static build that cannot be had is not worked around by linking dynamically: the image has no libc to link against.
9. **init reads the XFS superblock itself**, as it reads the data disk's, for the label, the size of the file system and its inode percentage, and tells an XFS disk from a blank one from anything else without `blkid`.

## Consequences

- A Node with a second disk and one `--storage` flag keeps its Machines' disks on XFS with reflinks, at the size of that disk. A Node without the extra disk, and a Node whose user-data has no `spec.storage`, boot as they did: the field is optional, and no code path of init runs without it.
- The first boot with a blank disk formats it, and a boot that finds the same disk again never does. A disk whose first MiB holds anything, another file system's superblock included, is never formatted; data only past the first MiB is not seen, and such a disk is formatted. The cost is that a disk with a damaged superblock is left alone and reported, not repaired.
- A disk that is not blank and not XFS is the operator's to wipe. Init does not offer a force.
- The mount over `mvmd/disks` hides what the data disk holds there. A Node that had Machines before the storage disk was added keeps their disks on the data disk, out of mvmd's sight, until the operator moves them.
- A disk's growth shows at the next boot. A Node that must grow its storage without stopping waits for a decision this one leaves out.
- The image is larger by `mkfs.xfs` and by the kernel's XFS and device-mapper code, and a build needs a C compiler, `make`, `pkg-config` and `ar` on the host, for the util-linux and userspace-rcu builds that the e2fsprogs build did not need.
- The release workflow's runner dependencies are not changed by this decision's pull request; whether the runner has what the build needs is for the release to find.
- With `XFS_ONLINE_SCRUB` off, an operator who wants an online scrub needs a kernel built with it and a `xfs_scrub` in the image: a new decision.

## Signs the decision was wrong

- A Node is found with its extra disk formatted when it held data, whichever way the disk got there: the blank test (one MiB of zeros) is too weak, and a stronger one, or an operator's explicit wipe, replaces it.
- Operators routinely need a disk found by a stable name (`by-id`, a serial number, a WWN) and cannot have one: the image then needs udev or a replacement for it, and `device` takes such a path.
- Operators routinely add a storage disk to a Node that already runs Machines and find the hidden files a trap: the mount moves to a migration init performs, or mvmd takes its disk directory as a setting and the mount over it goes.
- A disk's growth under a running Node is needed without a reboot: growth moves to a live action of the apply engine, and the class of `spec.storage` is reconsidered.
- A form other than XFS is needed (a thin-provisioned device-mapper layout, a raw device): `form` takes it, and this decision's single accepted value is what changes.
- A failure to prepare a disk is regularly missed in the log and costs a Node's Machines their disks: the failure refuses the boot, as the data disk's does.

## Errata

- 2026-11-15: paths moved in the image split ([ADR 0054](./0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)): `image/xfsprogs/` is `image/common/xfsprogs/`.
