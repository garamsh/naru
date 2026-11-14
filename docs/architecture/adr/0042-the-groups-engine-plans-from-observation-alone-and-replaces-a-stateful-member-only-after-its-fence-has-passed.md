# ADR 0042: the groups engine plans from observation alone and replaces a stateful member only after its fence has passed

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

orch keeps groups of microVMs on hosts it reaches through the host API (aurora#811, #113 phase O3). Three questions decide its shape. What the engine remembers between passes: a remembered intent survives an orch restart only if it is persisted, and then it can disagree with the hosts. When a rolling update, a drain or a lost host may remove a member: a bound has to be stated once. When a stateful member may be replaced: its data lives on one host, and a second instance beside a running first one is worse than a short outage. mvmd now ends a Machine's VMM when its own fence deadline lapses, even with mvmd down (garamsh/mvmd#747), and reports `fence_lapsed_at`.

## Decision

- **The plan depends only on observation.** A pass observes the store and the hosts, evaluates every member into a phase, plans actions and applies them; it keeps nothing for the next pass. The engine is constructed and run by orchd while it holds the leader lease.
- **One floor bounds every removal.** A running member is removed only while the available members stay at or above the larger of `minAvailable` and `count - maxUnavailable`; creation is bounded by `count + maxSurge`. A rolling update, a drain, a broken required label and a lowered count all use it. A group whose floor equals its count and whose `maxSurge` is 0 is refused, because no member could ever leave.
- **A stateful member is created `fenced=true` and its deadline is renewed by orch while its host is ready.** When its host is lost or not ready, no replacement is created until the deadline orch last armed has strictly passed or the host reports the fence lapsed. A stateful group may not surge; a voluntary move stops the member, deletes it, and only then creates its replacement.
- **Placement is a pure function** of the group, the hosts and the members already placed; apply is separate from it.
- **The engine defines the interfaces it consumes** (the host API, the store with compare-and-swap revisions, the clock, the events) and orchd's composition site supplies their adapters.

## Consequences

- Restarting or replacing orch loses nothing: the next leader plans from the same observation.
- One rule, tested at every machine change, bounds a rolling update, a drain and a re-placement.
- A lost stateful host costs its members up to `fenceTTL` of unavailability; that is the price of never running two.
- A stateful group is limited to `maxSurge` 0, so its updates take a member down before its replacement is created.
- Forgotten members stay in the store as records; a group removed from the store is not reconciled.

## Signs the decision was wrong

- Passes oscillate: the same member is created and retired in successive passes because observation lags what the previous pass did.
- A stateful member runs on two hosts at once, which would show that the deadline wait is shorter than the host's own fence, for instance through clock skew between orch and a host.
- Operators need a stateful rollout that never drops below its count, which `maxSurge` 0 cannot give.
- The tombstone records outgrow the store's comfortable size.
