# ADR 0092: the leader removes a member that was added and stays unstarted for the departure time, and does not count it as a member out until then

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#303. It amends item 6 of [ADR 0088](./0088-orch-compacts-defragments-and-bounds-its-etcd-by-a-share-of-the-data-disk-drops-events-by-age-removes-a-member-that-stays-out-for-it-to-rejoin-empty-and-init-grows-the-data-file-system-to-its-disk.md) by reference. The facts about etcd are those of `go.etcd.io/etcd/server/v3` v3.6.16, the version `orch/go.mod` pins.

## Context

- **A member that is added and has not started has no client URL in the member list**, and the leader's housekeeper cannot probe it. Item 6 of ADR 0088 counts a member as out when it does not answer, so such a member was out.
- **An unstarted voter counts toward the quorum.** `RaftCluster.VotingMembers` lists every member that is not a learner, started or not. A cluster of three that adds a fourth member needs three of four, and when that member never starts, losing one more member stops the cluster.
- **The first change for #303 (commit 47049f45b2285293e21d64952289d52702949344) stopped tracking such a member at all**, so that it did not count as a second member out and block the removal of a departed one. A join that failed after `MemberAdd` then left the member in the list for good, with the quorum cost above.
- **A removal of a member that is not part of the active quorum is not refused by etcd's strict reconfiguration check**: `mayRemoveMember` returns early for a member whose transport has never been active.

## Decision

1. **The leader tracks a voter with no client URL with a timer of its own**, started when the leader first sees it unstarted and dropped when it starts or leaves the list. A learner is not tracked: it takes no part in the quorum.
2. **Inside that window the member is not out.** It does not count against the "every other member answers" condition of item 6, so a departed member is removed while a joining one is still starting.
3. **Once the member has stayed unstarted for the departure time (`--depart-after`, item 6 of ADR 0088), the leader removes it**, after the same guarded empty transaction as any removal. The cluster-size minimum and the other-members condition do not apply to it: removing an unstarted voter lowers what the quorum needs and takes nothing from the members that run. It is removed before a departed member is, one removal per pass.
4. **The window is the departure time**, one setting for both.

## Rejected alternatives

- **A separate, shorter window.** A window of a few minutes would restore the fault tolerance sooner than 30, since a join is meant to start its etcd at once. It is not taken because no measured time from `MemberAdd` to a published client URL is recorded here to choose a value from, and a second window is a second flag and a second `spec.orch` field. The fleet tests set the one window short already. A measured join time well under the departure time, or an incident in which the cluster stopped inside the 30 minutes, is what would change this.
- **Not tracking the member** (the first change for #303). It leaves a voter that never starts in the list for good.
- **Tracking it as out, as before.** It counts against the one-member-out condition and keeps a departed member from being removed while a join is under way.

## Consequences

- A member added by a join that then failed is gone after the departure time, and the join can add it again.
- While a member joins, a departed one is still removed on time.
- A member that is slow to start for longer than the departure time is removed and adds itself again from the marker of ADR 0088.

## Signs the decision was wrong

- A member that starts normally is removed in the middle of its start: the window is shorter than a start takes.
- A cluster stops for want of a quorum inside the departure time because of a member that never started: the window is too long, and item 4 is the part to revisit.
