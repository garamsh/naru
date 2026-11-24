# Groups

What orch's groups engine does: how it keeps a group of microVMs at its declared count, template and placement through the host API, and what it decides on its own.

## Current decisions

- **The engine is the package `orch/internal/groups`.** It is a pure-Go engine in the module `orch/`. orchd constructs it with adapters for the host API (`orch/internal/hostclient`), the store and the events (`orch/internal/groups/etcd`) and the clock, and runs it while orchd holds the leader lease, as its `Reconciler`; [orch.md](./orch.md) §Groups says what each adapter does; leader gating is orchd's ([ADR 0042](./adr/0042-the-groups-engine-plans-from-observation-alone-and-replaces-a-stateful-member-only-after-its-fence-has-passed.md)).
- **Level-triggered.** Every pass observes (groups and members from the store, hosts and their microVMs from the host API, and the fence of each stateful member whose host answers), evaluates, plans and applies. The plan reads only that observation; nothing about earlier passes is kept, so a new engine continues what another started.
- **What an operator declares (`GroupSpec`).**
  - Identity and size: `name`, `count`, `region`.
  - Constraints: `requiredLabels` (a host must carry each), `preferredLabels` (key, value and weight; a host scores the weights it matches).
  - `template`: form, CPUs, memory, version, disks, `policy` (the egress and ingress rules), the `network` the members join with the `podPrefixBits` of each member's Pod prefix, `rootDiskBytes` (above zero for the image form) and, for an OS-form microVM, the node version and `userData`, its seed (`#cloud-config` when empty).
  - `rollout{maxUnavailable, maxSurge}` and `minAvailable`.
  - An OS-form template's `nodeVersion` is a release version or a boot-set name a host offers; the engine's placement, the host call that creates the microVM and the boot-set reconciler resolve it with one function ([ADR 0089](./adr/0089-orch-adds-the-boot-set-a-group-needs-to-the-hosts-that-place-it-from-a-release-store-it-is-configured-with.md), accepted).
  - `stateful` and `fenceTTL`.
- **A member** is the record of one microVM: `id` (`<group>-<n>`, also its machine ID), `group`, `host`, `network`, `podPrefix`, `templateHash`, a stored `phase` (pending or gone), `lastSeen`, the last fence deadline orch armed and, while the host refuses its create, `createFailures` and `retryAt`. The live phase (pending, running, draining, fenced-waiting, gone) is computed from observation on every pass.
- **What the engine keeps true.**

  | Behaviour | Rule |
  | --- | --- |
  | Count | A member whose microVM is gone, or whose host is lost, is replaced. |
  | Rolling update | A member whose template hash differs is retired; creation is bounded by `count + maxSurge`, and a running member is removed only while available members stay at or above the larger of `minAvailable` and `count - maxUnavailable`. |
  | Drain | A member on a draining host is retired by the same bound, so members leave within `minAvailable`. |
  | Required label | A member on a host that no longer holds the group's region or required labels is retired and placed elsewhere. |
  | Placement | Among hosts that are ready, not draining, in the region, carrying the required labels, with the node version in the host's window and with room: highest preferred score, then fewest members of the group, then most free memory, then lowest ID.. The room is orch's one account of a host, shared with Machines, and a member's record commits with the host's allocation record (`/naru/orch/host-allocations/<host>`), so a member and a Machine placed in one pass cannot both take the last of a host (decided, not built: [ADR 0110](./adr/0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md)). |
  | Node version | A template naming a node version the host does not list (its N and N-1) is refused for that host. |
  | Count lowered | Surplus members are retired by the same bound. |

- **A member's Pod prefix is its own** ([ADR 0049](./adr/0049-orch-keeps-the-networks-and-allocates-each-members-pod-prefix-by-compare-and-swap-and-the-leader-tells-every-host-its-networks-members-and-peers.md)).
  - The plan gives a new member the first subnet of `podPrefixBits` bits of the network's prefix that no member of that network that is not gone holds, across groups. A network orch does not keep, or one with no subnet left, holds the group.
  - Creating the record is an etcd transaction with a claim key under `/naru/orch/pod-prefix-claims/<network>/<prefix>`; losing it is a conflict, and the next pass plans again. The claim is deleted when the member is written as gone, only if it still names that member.
  - The engine reads the networks from the store. The network reconciler ([orch.md](./orch.md) §Networks) declares them on the hosts; a create that reaches a host before that fails and is made by the next pass.
- **Stateful members are fenced.**
  - Created with `fenced=true`; the engine renews the member's deadline through the host API while its host is ready, when less than half of `fenceTTL` is left, and records the deadline on the member.
  - A voluntary move (drain, template change) stops the member, then deletes it, then creates its replacement: a stateful group may not surge.
  - When the host is lost or not ready, the member is fenced-waiting and holds its place in the count. The replacement's Create comes only after the recorded deadline has strictly passed, or after the host reports `fenceLapsedAt` for that member.
- **A host is draining when the operator says so.** `groups.Host.Draining` is the `draining` flag of the host's inventory record ([orch.md](./orch.md) §Drain), which the host client copies on every pass whether or not the host answers; the engine's own rules above then apply unchanged, so a drain keeps the group's count, moves an image member off within `minAvailable`, moves a stateful member only after its stop and its fence, and places nothing on the host until `undrain`.
- **A decision to wait is not an error.** A placement refusal, an invalid spec, a fence wait and a rollout waiting for room are recorded as hold events, with the reasons each host was refused.
- **Events.** One event per applied action (with its reason), one per hold, and one `failed` event for an action that failed. A failed action does not stop the pass; the next pass plans again.
- **Creation order.** The member record is written before the host is asked, so a crash between them leaves a record whose microVM is absent, which the next pass finds gone. A create the host refuses keeps its record, counts the failure on it (`createFailures`) and puts the next attempt off to `retryAt`: the wait starts at the pass interval (10 s), doubles after each failure and stops at 10 minutes (`createBackoffMax`). The next attempt writes the same record, placed again, so a series of failures is one record and one `failed` event per attempt, not one record per pass; a success clears the count. Lowering `count` or a drain ends the record of a member that was never made, with no delete to a host.
- **Bounds on what a pass asks of the world.**
  - A call to a host has the deadline of its context alone (5 minutes, `callTimeout`); the HTTP client's own timeout is not a second one, so a create that outlasts it is not cut and made again.
  - The inventory is read once per pass, by the host list, and the calls of that pass use the hosts it returned; a host that is not in it is not admitted for the pass.
  - A host's tunnel and microVM probes (`hostclient.NetHosts`) answer with the last good answer until 3 consecutive probes have failed (`probeFailures`, K = 3), so the network reconciler keeps the host's peers and routes through one or two failed probes and drops them at the third; a success resets the count, and a host that has not answered once is reported as failed.
- **Forgotten members** are written as gone and are not deleted while their group lives, so a live group does not reuse their IDs. Deleting the group deletes their records with it, in one etcd transaction that holds only while the group exists, every member of it is gone and no member record changed since they were read; a group applied again under the name numbers its members from 1, and its first member takes the template's first address (garamsh/naru#366).

## Rationale

- [ADR 0042](./adr/0042-the-groups-engine-plans-from-observation-alone-and-replaces-a-stateful-member-only-after-its-fence-has-passed.md): why the plan reads only observation, why the rollout bound is one floor, why a stateful group may not surge, and the signs it was wrong.
- [host.md](./host.md): the host API the engine drives; mvmd's per-Machine fence (garamsh/mvmd#747) is what a fenced member relies on.

## Open questions

- **Clock between orch and the hosts.** The wait compares the deadline orch armed with orch's clock, and the host ends the VMM by its own. No margin is added; the engine does not yet know the skew it must tolerate.
- **A group that is deleted.** The engine lists the groups the store holds, so the members of a removed group are not reconciled. The API therefore refuses to delete a group that has a member that is not gone: lowering `count` to zero removes them first. The engine deletes the machine of a gone member when its lost host returns only while the member's record and group exist, so a machine a lost host still runs after its group was deleted stays there, as it did before the records were ended; a group applied again under the name may then give that member's ID and address to a new member.
- **Tombstones.** Forgotten members of a live group stay in the store; compaction waits until their number matters.
- **Repeated holds.** A wait that lasts is recorded on every pass; the events adapter writes it once per 10 minutes. The adapter's memory is per leader, so a new leader writes it again.
- **Event log.** The inventory's event log keeps the newest 20000 events (`maxEvents`), trimmed on each group event; admin events written in a transaction do not trim it themselves. See [orch.md](./orch.md).
- **Group and event records.** The store interface the engine consumes lists groups and members and writes members; the API layer (`orch/internal/groupsvc`) writes groups and reads events.
