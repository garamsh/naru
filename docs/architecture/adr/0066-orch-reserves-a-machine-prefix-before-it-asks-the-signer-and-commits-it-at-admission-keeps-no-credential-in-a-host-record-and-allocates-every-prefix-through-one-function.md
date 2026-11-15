# ADR 0066: orch reserves a Machine prefix before it asks the signer and commits it at admission, keeps no credential in a host record and allocates every prefix through one function

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#192, part of goal garamsh/naru#197. It adds to [ADR 0059](./0059-orch-admits-a-host-that-boots-from-an-install-seed-allocates-its-machine-prefix-and-builds-its-configuration-from-one-fleet-profile.md) and [ADR 0049](./0049-orch-keeps-the-networks-and-allocates-each-members-pod-prefix-by-compare-and-swap-and-the-leader-tells-every-host-its-networks-members-and-peers.md), and does not change their bodies; ADR 0059 carries an erratum.

## Context

- **Admission asked the signer before it claimed a prefix.** Two admissions raced for the same free /24: both had a certificate issued, one lost the allocation and its certificate was wasted, and a full pool was found only after the signer had been called.
- **Writes of an enrollment, an admission and a heartbeat were not guarded against each other.** A host that enrolled again while an operator admitted it could leave a pending record beside the admitted host.
- **A host record held the built `naru.yaml`.** That file carries the profile's registry password, so every reader of etcd and every snapshot held a copy of a secret the API promised never to read back.
- **Four functions chose a prefix**: the Machine allocator, the Pod allocator, and two checks of overlap. They disagreed on a prefix of another length, and a Pod prefix could overlap one already claimed in the same network.
- **A poll told a caller whether a host ID existed** (a bad proof and an unknown ID had different answers), and nothing bounded polls.

## Decision

1. **Admission of a seed-booted host is reserve, sign, commit.** Orch reads the allocations and writes the lowest free /24 as a reservation with `ReservedUntil` 2 minutes ahead (`reservationTTL`), in a transaction that fails if any allocation was written since the read. Only then does it call the signer. It commits in one transaction that requires the pending record to exist, the host to be absent and the reservation to be as it wrote it, and it writes the confirmed allocation, the host, and removes the pending record. A failure after the reservation releases it. An expired reservation is free and any admission may take it over. A host without hardware keeps today's order, since it takes no prefix.
2. **Writes that create or replace one key are compare-and-swap.** An enrollment write requires the host to be absent and the pending record to be as read, or to be absent for a first enrollment; a failure is `ErrAlreadyAdmitted` or `ErrConflict`, and `Enroll` retries a raced write 4 times. A network and a group are created only if absent and replaced only if unchanged since the read (`ErrConflict`, 409).
3. **A host record stores no `config`.** Orch builds the `naru.yaml` for each answer to a poll or an enrollment from the host record and the profile as it is then. A profile that is missing answers `ErrNoProfile` (409); one that no longer checks answers `ErrConflict` (409). A profile change therefore reaches a host that has not yet fetched its configuration. `GET /v1/profile` is built from an allow-list of fields, so a field added to the profile is not shown until it is listed.
4. **The migration is a one-shot rewrite run by the leader after it wins the election.** For each host record it reads, drops `config` and writes with a compare on the record's revision; a record that changed meanwhile is read again. It logs the count, it is idempotent, and a leader that finds nothing to do writes nothing. The decoder still accepts a `config` field, so a record not yet rewritten reads. Snapshots taken before the rewrite still hold the old copies until they are replaced (8 are kept).
5. **There is one allocator, `orch/internal/prefixes`.** `Allocate(space, bits, claimed)` returns the first prefix of the length that overlaps no claim, and `Overlaps` is the one overlap test. The Machine allocator and the Pod allocator call it. A claim of another length counts.
6. **The Machine space and the Pod spaces stay apart, with no cross-check.** The Machine pool is the profile's, the Pod prefix is inside a network's own prefix; the two are different address spaces on a host, reached through different interfaces, and a check between them would reject a layout that works. If a deployment ever uses one range for both, this is the decision to revisit.
7. **A poll answers one way for a caller without proof.** An unknown ID, a signature that does not verify and a stale signature are 404 with one identical body. A source over its poll rate (10 per second, burst 100, in memory per member) is 429. A pending host with a valid proof is 200. A request body over its bound is 413, not 400.

## Consequences

- **No certificate is issued for an admission that cannot get a prefix.** A full pool is 409 before the signer is called.
- **Two admissions never hold overlapping prefixes**, and two concurrent creates of one network or group have one winner.
- **A reservation that was never committed holds a prefix for at most 2 minutes** after its holder died.
- **A stored host holds no password.** The credential lives in the profile alone.
- **Each answer to a poll is built again**, which costs a read of the profile and a render; a host polls once a minute.
- **A known limit.** A change of a network's prefix and a concurrent creation of a group in that network are two keys; they are not made atomic. Each key's own create or replace is guarded; the pair is not.
- **The poll limit is per member**, as the enrollment limit is, so a caller over several members gets the sum.

## Signs the decision was wrong

- Admissions are common enough that the 2-minute reservation holds up the pool, which would say the TTL belongs in configuration.
- A deployment overlaps the Machine pool with a network's range, which would say decision 6 needs a check.
- The pair of keys in the known limit is hit in practice, which would say a network and its groups need a transaction across both.
