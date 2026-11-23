# ADR 0102: orch records the Machine prefixes every host reports, and admits a joined host only while every admitted host has one recorded

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#262. It extends [ADR 0059](./0059-orch-admits-a-host-that-boots-from-an-install-seed-allocates-its-machine-prefix-and-builds-its-configuration-from-one-fleet-profile.md) and [ADR 0066](./0066-orch-reserves-a-machine-prefix-before-it-asks-the-signer-and-commits-it-at-admission-keeps-no-credential-in-a-host-record-and-allocates-every-prefix-through-one-function.md) by reference.

## Context

- **The pool knew only the prefixes orch gave.** The allocation records (`/naru/orch/allocations/<prefix>`) were written only for a host that joined from an install seed. A host enrolled with a configuration of its own, such as a control host whose `naructl gen node --machine-prefix` set `10.200.1.0/24`, was admitted with no allocation, so the pool gave a joined host that same `10.200.1.0/24`, and the joined host's mvmd then held a prefix the control host's mvmd held.
- **The host knows what it holds.** `GET /v1/host` carries `mvmd.machinePrefixes`, from mvmd's runtime information ([ADR 0099](./0099-a-group-template-lists-its-members-addresses-by-number-orch-places-a-member-on-a-host-whose-machine-prefix-holds-its-address-and-mvmd-gives-it.md) item 3), for every host whatever way it was configured.

## Decision

1. **The leader records what each admitted host reports.** A recorder, run beside the groups engine on the leader's guarded store, reads every admitted host's `machinePrefixes` once per pass (`--network-interval`) and writes each prefix as a confirmed allocation of that host, the record `Reserve` and `Admit` already write for a joined host. A host that already holds the prefix is left as it is, and a host's own reservation or an expired one is taken over. A host that does not answer is skipped, and a host that is not admitted holds nothing.
2. **A prefix another host holds is never overwritten.** The recorder appends the event `host.prefix-conflict` naming the host and the prefix, once per leader term, and leaves the record with the first host.
3. **A joined host is admitted only while every admitted host holds a recorded prefix.** Before it reserves a prefix, `Admit` of a host with hardware lists the admitted hosts and the confirmed allocations; a host with none is 409 naming it. The admission is not issued a certificate, and the host stays pending. The operator removes a host that never reports, or waits for it.
4. **Removing a host frees every prefix it holds**, those it reported as well as the one orch gave.
5. **A host enrolled with its own configuration is unchanged.** Its record keeps an empty `machinePrefix`, which is what tells orch it has no configuration to build; the allocation is a separate record.

## Options considered

- **The enrollment request carries the host's prefix.** It changes the enrollment protocol, does not reach a host admitted before the change, and states what the host intends and not what its mvmd holds.
- **The allocation reads the hosts' reports itself.** It puts a call to every host inside the admission and fails it when one is slow; recording first keeps the admission a read of orch's own records.
- **Skip the gate and rely on the next pass.** An admission in the seconds before the first pass could still hand out a held prefix.

## Consequences

- Hosts admitted before this change are recorded by the first pass of a leader that has it, with no operator action.
- A host that is down, or runs a host API that reports no prefix, blocks the admission of joined hosts until it is removed or reports. The 409 names it.
- Overlap of a different-sized prefix is not detected: the records are keyed by the exact prefix.

## Signs the decision was wrong

- Admissions that wait on hosts that are down more often than they save a collision.
- A prefix reported by a host that is not one mvmd serves (a stale report).
