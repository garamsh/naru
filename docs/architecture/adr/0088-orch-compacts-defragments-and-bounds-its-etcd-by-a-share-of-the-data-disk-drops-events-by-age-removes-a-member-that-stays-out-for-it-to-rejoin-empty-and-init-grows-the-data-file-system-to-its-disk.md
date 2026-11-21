# ADR 0088: orch compacts, defragments and bounds its etcd by a share of the data disk, drops events by age, removes a member that stays out for it to rejoin empty, and init grows the data file system to its disk

> Status: accepted
> Date: 2026-11-21

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#278, part of goal garamsh/naru#262. It extends [ADR 0071](./0071-an-orch-leader-fences-its-etcd-writes-and-its-host-calls-with-the-revision-of-its-election-key-and-a-host-refuses-a-lower-token.md) (the leader is the one that acts) and the event log of [orch.md](../orch.md).

## Context

- **aurora's orch cluster is at two of three members** (#278). The data disk of a control host is 2 GiB. etcd was started with its defaults: no compaction, a backend quota of 2 GiB (the whole disk), and as many WAL and snapshot files as it makes. A member filled its disk, raised the no-space alarm, and could not start again, because its data directory had no room for the WAL it replays.
- **A full etcd is not fixed by deleting keys.** A delete adds a revision; the space returns only after a compaction and a defragmentation, and the cluster stays read-only while the alarm is armed.
- **The event log is bounded by count only** (20000), so a host that logs large events, or a log left alone for a long time, is bounded by nothing a person can reason about.
- **A member that lost its disk or its data cannot rejoin by restarting**: the cluster still lists it, so adding it again is refused; only a removal followed by a start with an empty data directory works.
- **The data disk of a Node is a sparse image or a volume an operator enlarges.** The file system on it keeps the size it was made at.

## Decision

1. **etcd compacts by time.** Each member runs with `AutoCompactionMode=periodic` and `AutoCompactionRetention=10m`: it keeps ten minutes of history, which is far more than a watcher of orch's inventory lags by, and compacts every tenth of that.
2. **etcd keeps a quota that is a share of the disk.** The backend quota is one eighth of the file system that holds the data directory, within 128 MiB and 2 GiB. A defragmentation writes a second copy of the database, and the WAL sits beside both, so at one eighth the worst case, twice the quota, plus the WAL files, fits. aurora's 2 GiB disk gets 256 MiB. A smaller disk than 1 GiB is still given 128 MiB: a quota below it would refuse the inventory of a small fleet.
3. **etcd keeps two WAL files and two snapshots** (`MaxWalFiles`, `MaxSnapFiles`), so the log and the snapshots stop growing with the age of the member.
4. **The leader defragments.** Once a minute the leader's housekeeper (`orch/internal/member`) lists the members and asks each for its status. When every member answers, it defragments, one member at a time, each whose database is at least 32 MiB and at least half free pages, and then disarms the no-space alarm of a member whose database is below the quota. A defragmentation blocks its member, so it is never run while a member is out: the cluster would then have one fewer member to answer.
5. **Events are dropped by age as well as by count.** When an event is appended, those older than 7 days are deleted by one range delete on their keys (which sort by time), then the count bound of 20000 applies as before. Both are done in the appending transaction pattern the store already uses, so no second writer is needed.
6. **A member that stays out is removed by the leader and rejoins empty, automatically.** When exactly one member has been unreachable for 30 minutes as seen by the same leader, the cluster has three members or more, and every other member answers, the leader removes it with `MemberRemove`. The cluster keeps its quorum and its fault tolerance minus one while it is out. At start, a member that has data, was given a join URL (`--join`, from `spec.orch.join`) and finds that the cluster no longer lists its peer URL, deletes its member directory and starts as a new member from its peers. A cluster that cannot be reached leaves the directory alone. The removal is refused, and logged, when it would not leave a quorum: two members out are never both removed.
7. **Init grows the data file system to its disk at boot.** After it mounts the data disk, init reads the ext4 superblock, and when the device is larger than the file system it issues `EXT4_IOC_RESIZE_FS` on the mount point with the new block count. The growth is online, needs no unmount and keeps the data. A failure is logged (`data disk not grown`) and is not a refusal: a host that runs on the size it had is better than one that does not boot. A success is logged as `data disk file system grown`.
8. **The default data disk of a control host is 8 GiB sparse** in the release's harness and its release notes, where the quota is 1 GiB. The constant is in the image tooling and changes in a follow-up of the image's owner; this decision records the size the quota is derived for.

## Rejected alternatives

- **An operator command to remove and re-add a member** (`naructl orch member ...`). It is the right shape for a one-off repair, but aurora's host is headless, the failure repeats on every control host the same way, and a command nobody runs on time leaves the quorum at two. The automatic form is guarded where the command would be: by the quorum check. The cost is that an automatic remove can act on a mistaken view; the leader's own count of 30 minutes and the all-others-answer condition are what bound that.
- **A fixed quota** (etcd's advised 8 GiB, or its 2 GiB default). On a 2 GiB disk it is the disk; a share of the disk keeps the room a defragmentation and the WAL need.
- **Revision-count compaction.** It depends on the write rate; a quiet fleet would keep history for weeks.
- **Defragmenting all members at once.** It blocks every member together and ends the cluster's availability for the length of the copy.
- **Removing and re-adding the member through the API for a full member that is still running.** Compaction and defragmentation make it unnecessary; the removal is for a member that cannot start.
- **Failing the boot when the file system cannot be grown.** The host would be lost for a size problem.
- **`resize2fs` in the image.** The image carries no e2fsprogs; the ioctl is the kernel's own interface and is what `resize2fs` calls for an online file system.

## Consequences

- A control host's etcd stays within about a quota of its data disk, and a member whose database was large is defragmented without an operator.
- A member that is merely slow for 30 minutes, or is down for a maintenance of that length, is removed and rejoins empty, copying the cluster's state from its peers. That is correct and costs a copy.
- A member whose unit has only a static `--initial-cluster` and no `--join` gets no automatic rejoin: it is removed by the leader and does not find the cluster again at start. The images write `--join` from `spec.orch.join`; a hand-made unit must do the same.
- The event log's age bound removes events older than 7 days even when fewer than 20000 exist.
- A data disk enlarged between boots is usable without a reformat.

## Signs the decision was wrong

- A leader removes a healthy member, for example one behind a partition the leader alone sees: the removal would need a second observer, or a longer time.
- The inventory's size nears the quota on disks of 1 GiB: the minimum of 128 MiB or the share would have to change.
- An operator needs to remove a member at once: the command rejected above would then be needed beside the automatic form.
- The ioctl is refused on the file systems hosts use: init would then need a different growth path or a documented manual one.
