# ADR 0073: mvmd's disks, snapshots and run directories are on the first storage disk through links, while its state directory stays on the data disk

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#219, part of goal garamsh/naru#197. It amends [ADR 0027](./0027-a-node-offers-an-extra-disk-as-xfs-storage-for-mvmds-disks-formatted-when-blank-mounted-hardened-and-grown-at-boot.md) item 6 (an erratum there points here), and keeps the obligations of [ADR 0003](./0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md).

## Context

- **mvmd refused to start on a Node with a storage disk.** ADR 0027 mounted the disk over `/var/lib/naru/mvmd/disks` only. mvmd's `state_dir` and the Machines' run directories (`machines` under it) stayed on the data disk, so its start-up probe, a rename from the run directories into `snapshots/staging`, failed with "its disk directory and snapshot directory are on different file systems", and every host API call that needs mvmd answered `503`. aurora met it on v0.1.0-dev.11 on three control hosts.
- **`state_dir` cannot move to the storage disk.** It also holds the Image Store, `images/`, which mvmd requires on a file system with fs-verity and checks at start (`daemon/internal/app/config.go`, `app/images.go`; ADR 0003's row). XFS in linux 6.18.63 has no fs-verity code.
- **mvmd's layout at the pinned commit `b1a60cd`, read for this decision:**
  - `disks/` and `snapshots/` are `<state_dir>/disks` and `<state_dir>/snapshots`, not settings. `supervision.dir` is a setting, and must be `machines` under `state_dir` or outside it.
  - `conversions/` is `<state_dir>/conversions`, fixed, and holds only the converter VMs' supervisor directories; it stays on the data disk.
  - Nothing refuses a directory that is a link. mvmd `Lstat`s and opens with `O_NOFOLLOW` the entries inside `disks/` and `snapshots/`, never the directories; `MkdirAll` and `fsync` of a directory follow a link, and `MkdirAll` fails on a dangling one.
  - Two mounts of one XFS do not pass the rename probe (EXDEV), but a link into one mount does.
  - The run directories need no exec on their file system: the supervisor bind-mounts a run directory `noexec` itself, and the VMM and supervisor executables are in `/bin`. ADR 0027's `noexec` mount is therefore kept.

## Decision

1. **`state_dir` stays `/var/lib/naru/mvmd` on the data disk.** `images/`, the database and `conversions/` stay there, with fs-verity.
2. **The first storage disk is mounted once, at `/var/lib/naru/mvmd-storage`** (`wire.MvmdStorageMount`), outside `state_dir`, with ADR 0027's `nodev,nosuid,noexec` and mode `0700`. `gen node --storage` and the host profile write it as the first entry's `mountPoint`; the others keep init's default.
3. **Init links mvmd's state into that mount.** After mounting the disk at `wire.MvmdStorageMount`, init makes `disks/` and `snapshots/` there (mode `0700`) and a link `<state_dir>/disks` and `<state_dir>/snapshots` to each. An empty directory at a link's place, which is what dev.11's mount point left, is replaced; a link that is right is left; a directory that holds files, or a link to elsewhere, is logged at error level with its path, saying mvmd will refuse to start, and the boot goes on, as for any storage failure in ADR 0027 item 4.
4. **mvmd's `supervision.dir` is `/var/lib/naru/mvmd-storage/machines`** when the Node has storage, and is left out otherwise. mvmd creates it. The run directories, the disks and the snapshots are then on one XFS with reflinks.
5. **Without storage nothing changes.**
6. **Upgrade from dev.11.** A Node that had storage over `mvmd/disks` never ran mvmd, because of the refusal above, so it has no Machines to keep. The new layout comes only from user-data written by dev.12's `naructl gen node` or `gen host`. A Node upgraded in place keeps dev.11's user-data, which has the old `mountPoint` and no `supervision.dir`, so init still mounts the disk over `mvmd/disks`, links nothing, and mvmd keeps refusing until the operator writes new user-data and applies it. Init does not map the old mount point to the new one, since that would hide what the operator wrote. Once the disk is mounted at the new point, files at its root, which the disk still holds, are not migrated: they stay there, unused. A Node whose data disk holds Machines' disks in a real `mvmd/disks` directory keeps them there, and init refuses the link as item 3 says; moving them is the operator's.

## Consequences

- A Node with a storage disk runs mvmd and creates, snapshots and resumes Machines on the XFS. A QEMU test shows it (image.md).
- Two directories of `state_dir` are links, which mvmd was not written for. The layout rests on the checks above at `b1a60cd`, and a mvmd that refuses a link as a directory ends it.
- `snapshots/` and `disks/` are the XFS's, so a snapshot's state files move by rename and its disks by reflink.

## Signs the decision was wrong

- mvmd refuses `disks/` or `snapshots/` as a link at a later commit, or fails a Put across the link. A mvmd setting for the disks and snapshots roots would then replace the links, which is the follow-up that retires them.
- Operators hit the refusal of a non-empty `mvmd/disks` often enough that init should migrate it.

## Alternatives rejected

- **`state_dir` on the XFS.** The Image Store needs fs-verity, which XFS in naru's kernel lacks.
- **Two mounts of the XFS, over `disks/` and `snapshots/`.** The rename probe fails with EXDEV across mounts of one file system.
- **An ext4 storage disk with verity.** It loses ADR 0027's constant-time reflink copy.
- **Waiting for mvmd to take the roots as settings.** It blocks the release, and the links are retired when it does.

## Errata

- 2026-11-16: Item 3 links `disk-snapshots/` too: init makes `disk-snapshots/` on the XFS (mode `0700`) and a link `<state_dir>/disk-snapshots` to it, as for `disks/` and `snapshots/`. mvmd at `b1a60cd` keeps `disk-snapshots` under `state_dir` and refuses to start unless its `staging` is on the file system of `disks` (`daemon/internal/app/storage.go`, `checkDisksSameFileSystem`): "the disk snapshot directory %s is not on the file system of the disk directory %s, so a disk could not be cloned into a snapshot". With `disk-snapshots/` left on the data disk, mvmd never started on a Node with a storage disk. The body above is not edited.
- 2026-11-21: Items 3 and 6 no longer make init refuse a real `mvmd/disks`, `mvmd/snapshots` or `mvmd/disk-snapshots` directory that holds entries. Init moves each entry into the storage target (copied sparse under a temporary name, fsynced, renamed, the source renamed to a tombstone and removed) before it replaces the directory with the link, and a boot that was interrupted finishes the move. Only a name that is already taken in the target with other content is refused, naming both paths, and nothing is overwritten. A dev.11 host whose crash-looping mvmd left an entry there reaches the new layout without the operator moving anything.
