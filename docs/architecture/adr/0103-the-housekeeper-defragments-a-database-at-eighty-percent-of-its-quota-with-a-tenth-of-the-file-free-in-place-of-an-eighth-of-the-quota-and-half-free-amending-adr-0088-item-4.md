# ADR 0103: the housekeeper defragments a database at 80% of its quota with a tenth of the file free, in place of an eighth of the quota and half free, amending ADR 0088 item 4

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#357. It amends item 4 of [ADR 0088](./0088-orch-compacts-defragments-and-bounds-its-etcd-by-a-share-of-the-data-disk-drops-events-by-age-removes-a-member-that-stays-out-for-it-to-rejoin-empty-and-init-grows-the-data-file-system-to-its-disk.md) by reference.

## Context

- **ADR 0088 item 4 defragmented a member whose database is at least an eighth of the quota and at least half free pages.** The second condition never holds for a database that takes a steady write under periodic compaction: etcd reuses the pages compaction frees, so the file stops growing at a size it has already reached.
- **Measured** (etcd 3.6.16, one embedded member, quota 8 MiB, compaction retention 5 s, a 400 KiB value of one key written every second, 240 writes; a scaled copy of the housekeeping fleet test): the file was 7,831,552 bytes from the 20th write on, 4,956,160 of them in use. That is 93% of the quota with 37% of the file free. No write was refused and the old rule never fired.
- **A file at that size is one burst from the quota's alarm**, which holds the cluster read-only until the housekeeper has freed the space ([ADR 0088](./0088-orch-compacts-defragments-and-bounds-its-etcd-by-a-share-of-the-data-disk-drops-events-by-age-removes-a-member-that-stays-out-for-it-to-rejoin-empty-and-init-grows-the-data-file-system-to-its-disk.md) item 3), while a third of it is free to take.

## Decision

1. **One rule: a member's database is defragmented when its file is at least `DefragHighWater` percent of the quota and at least `DefragReclaimable` percent of the file is free pages.** `DefragHighWater` is 80 and `DefragReclaimable` is 10, named constants of `orch/internal/member`. `ShouldDefrag` is the one place the rule is held; it replaces the old rule and nothing stands beside it.
2. **The rest of item 4 stands**: one member at a time, only while every member answers, and the alarm's own condition for defragmenting a member whose free pages alone keep it armed.
3. **Nothing re-triggers on itself.** A defragmentation leaves the file the size of its live data, with no reclaimable share, so the rule is false until the file has grown and freed pages again.

## Options considered

- **Keep the old rule and lower the free share to a third.** It would still not fire on a database well below the quota with many free pages, and would fire on a small one for no gain; the high-water mark states what matters, the distance to the quota.
- **Defragment on a schedule.** It pauses a member whether or not it frees anything.

## Consequences

- A database that levels off near its quota is defragmented once it passes 80% with a tenth of it free, and no longer sits near the alarm.
- A small database in a large quota is never defragmented by this rule: below 80% of the quota there is nothing to gain.
- The fleet tests assert both sides: a defragmentation at the high-water mark, and none below it.

## Signs the decision was wrong

- A defragmentation pause that cluster members feel at 80% of a large quota: the high-water mark takes a bound in bytes beside the percent.
- A database whose live data alone is over 80% of the quota: no rule frees it, and the quota is raised ([ADR 0088](./0088-orch-compacts-defragments-and-bounds-its-etcd-by-a-share-of-the-data-disk-drops-events-by-age-removes-a-member-that-stays-out-for-it-to-rejoin-empty-and-init-grows-the-data-file-system-to-its-disk.md) item 3's report).
