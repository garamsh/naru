# Groups

What orch's groups engine does: how it keeps a group of microVMs at its declared count, template and placement through the host API, and what it decides on its own.

## Current decisions

- **The engine is the package `orch/internal/groups`.** It is a pure-Go engine in the module `orch/`. orchd constructs it with adapters for the host API (`orch/internal/hostclient`), the store and the events (`orch/internal/groups/etcd`) and the clock, and runs it while orchd holds the leader lease, as its `Reconciler`; [orch.md](./orch.md) §Groups says what each adapter does; leader gating is orchd's ([ADR 0042](./adr/0042-the-groups-engine-plans-from-observation-alone-and-replaces-a-stateful-member-only-after-its-fence-has-passed.md)).
- **Level-triggered.** Every pass observes (groups and members from the store, hosts and their microVMs from the host API, and the fence of each stateful member whose host answers), evaluates, plans and applies. The plan reads only that observation; nothing about earlier passes is kept, so a new engine continues what another started.
- **What an operator declares (`GroupSpec`).**
  - Identity and size: `name`, `count`, `region`.
  - Constraints: `requiredLabels` (a host must carry each), `preferredLabels` (key, value and weight; a host scores the weights it matches).
  - `template`: form, CPUs, memory, version, disks, network policy and, for an OS-form microVM, the node version.
  - `rollout{maxUnavailable, maxSurge}` and `minAvailable`.
  - `stateful` and `fenceTTL`.
- **A member** is the record of one microVM: `id` (`<group>-<n>`, also its machine ID), `group`, `host`, `templateHash`, a stored `phase` (pending or gone), `lastSeen` and the last fence deadline orch armed. The live phase (pending, running, draining, fenced-waiting, gone) is computed from observation on every pass.
- **What the engine keeps true.**

  | Behaviour | Rule |
  | --- | --- |
  | Count | A member whose microVM is gone, or whose host is lost, is replaced. |
  | Rolling update | A member whose template hash differs is retired; creation is bounded by `count + maxSurge`, and a running member is removed only while available members stay at or above the larger of `minAvailable` and `count - maxUnavailable`. |
  | Drain | A member on a draining host is retired by the same bound, so members leave within `minAvailable`. |
  | Required label | A member on a host that no longer holds the group's region or required labels is retired and placed elsewhere. |
  | Placement | Among hosts that are ready, not draining, in the region, carrying the required labels, with the node version in the host's window and with room: highest preferred score, then fewest members of the group, then most free memory, then lowest ID. |
  | Node version | A template naming a node version the host does not list (its N and N-1) is refused for that host. |
  | Count lowered | Surplus members are retired by the same bound. |

- **Stateful members are fenced.**
  - Created with `fenced=true`; the engine renews the member's deadline through the host API while its host is ready, when less than half of `fenceTTL` is left, and records the deadline on the member.
  - A voluntary move (drain, template change) stops the member, then deletes it, then creates its replacement: a stateful group may not surge.
  - When the host is lost or not ready, the member is fenced-waiting and holds its place in the count. The replacement's Create comes only after the recorded deadline has strictly passed, or after the host reports `fenceLapsedAt` for that member.
- **A decision to wait is not an error.** A placement refusal, an invalid spec, a fence wait and a rollout waiting for room are recorded as hold events, with the reasons each host was refused.
- **Events.** One event per applied action (with its reason), one per hold, and one `failed` event for an action that failed. A failed action does not stop the pass; the next pass plans again.
- **Creation order.** The member record is written before the host is asked, so a crash between them leaves a record whose microVM is absent, which the next pass finds gone. A create the host refuses forgets its record at once.
- **Forgotten members** are written as gone and never deleted; their IDs are not reused.

## Rationale

- [ADR 0042](./adr/0042-the-groups-engine-plans-from-observation-alone-and-replaces-a-stateful-member-only-after-its-fence-has-passed.md): why the plan reads only observation, why the rollout bound is one floor, why a stateful group may not surge, and the signs it was wrong.
- [host.md](./host.md): the host API the engine drives; mvmd's per-Machine fence (garamsh/mvmd#747) is what a fenced member relies on.

## Open questions

- **Clock between orch and the hosts.** The wait compares the deadline orch armed with orch's clock, and the host ends the VMM by its own. No margin is added; the engine does not yet know the skew it must tolerate.
- **A group that is deleted.** The engine lists the groups the store holds, so the members of a removed group are not reconciled. The API therefore refuses to delete a group that has a member that is not gone: lowering `count` to zero removes them first.
- **Tombstones.** Forgotten members stay in the store; compaction waits until their number matters.
- **Repeated holds.** A wait that lasts is recorded on every pass; the events adapter writes it once per 10 minutes. The adapter's memory is per leader, so a new leader writes it again.
- **Group and event records.** The store interface the engine consumes lists groups and members and writes members; the API layer (`orch/internal/groupsvc`) writes groups and reads events.
