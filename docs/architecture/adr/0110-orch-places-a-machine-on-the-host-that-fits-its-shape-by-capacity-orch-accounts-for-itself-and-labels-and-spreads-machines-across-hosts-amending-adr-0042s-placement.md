# ADR 0110: orch places a Machine on the host that fits its shape, by capacity orch accounts for itself and by labels, and spreads Machines across hosts, amending ADR 0042's placement

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#373 (goal 3). It decides how [ADR 0108](./0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md)'s shape becomes a host, and when [ADR 0109](./0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md)'s moves happen.

It amends by reference [ADR 0042](./0042-the-groups-engine-plans-from-observation-alone-and-replaces-a-stateful-member-only-after-its-fence-has-passed.md)'s bullet "Placement is a pure function": the room a group's placement sees is item 2's shared account, and a member's record is committed with its host's allocation record (item 2). The planner stays a pure function of what it observes, the allocation records now among it.

## Context

- **What a host reports.** `GET /v1/host` reports the host's `capacity` and what is `free` (CPU, memory, state disk), its region and labels, its mvmd Driver and its Machine prefixes ([host.md](../host.md)). mvmd measures the totals at each call and admits no call by capacity: overcommit and reserving for the host are the client's policy (garamsh/mvmd `runtime-api.md` §Observation, "mvmd admits no call by capacity", at `2a07ba313c0a2c424ba9badf40ea9c36ec670e14`).
- **What `free` lags.** It subtracts only running Machines. A Machine placed but not yet created, a stopped Machine whose volumes are kept, and a move's reserved target are all invisible in it, so two placements in one pass would both see the same free memory.
- **Labels exist.** A host has a region and up to 64 labels that an operator sets ([orch.md](../orch.md) §Region and labels). Whether a host is nested in a VM or bare metal, and which GPUs it has, are facts the host can read about itself.
- **Precedents.**
  - **Fly.io.** flaps "runs a quick best-fit ranking over the workers with space" in the requested region, and a request that does not fit fails rather than wait (fly.io/blog/carving-the-scheduler-out-of-our-orchestrator).
  - **E2B.** The API samples orchestrator nodes "best-of-K" by CPU and hugepage load (e2b-dev/runtime `docs/ARCHITECTURE.md` at `30a39cf3364197c162576f3823bb7e927f6c82d5`).
  - **Ubicloud.** Its allocator filters hosts by the request and scores them. It keeps non-GPU VMs off GPU hosts with a filter and a score penalty, and it requires free 1 GiB hugepages for the VM's memory (ubicloud/ubicloud `scheduling/allocator.rb` at `8417580759fd085fa20031dde0ba1f6ae1f3152f`).
  - **OpenNebula.** It filters by `SCHED_REQUIREMENTS` and ranks the hosts that pass (docs.opennebula.io/7.4, scheduler overview).

## Decision

1. **Orch accounts for capacity itself.** A host's allocatable CPU, memory and disk are its reported `capacity`, less a host reserve (`--host-reserve-cpu`, default 1, and `--host-reserve-memory`, default 2 GiB), less what orch has given away on that host. What orch has given away is the resources and volumes of every Machine bound there, running or stopped, every reservation held there, and the disk of every snapshot kept there, a snapshot of a Machine that has moved or been deleted included (the size its host reports for it). Every other microVM the host reports, a group's member included, is counted by the resources the host reports for it. Memory and disk are never overcommitted, and neither is CPU in `v1alpha1`. A Machine's GPUs are counted as devices ([ADR 0111](./0111-a-gpu-machine-runs-on-the-cloud-hypervisor-driver-with-whole-devices-by-vfio-on-a-bare-metal-host-pinned-to-the-devices-numa-node-with-hugepages.md)).
2. **A placement commits only against the account it read.** Each host has an allocation record (`/naru/orch/host-allocations/<host>`). A binding or a reservation is written in one guarded transaction with that record's new sum, and only while the record is the one the plan read. So two placements never both take the last of a host. The groups engine commits its placements through the same record, in the same transaction as its member record, for as long as groups exist (#373 §2 orders their review), so a Machine and a group member placed in one pass cannot both take the same memory. A move's reservation on its target is released at the move's commit or abort.
3. **Hosts report the facts placement needs, as labels orch keeps apart from the operator's.**
   - **Reported by the host.** `naru.garam.sh/virtualization` is `metal`, or `nested` when the CPU reports the `hypervisor` flag. The GPU inventory is reported by model and count ([ADR 0111](./0111-a-gpu-machine-runs-on-the-cloud-hypervisor-driver-with-whole-devices-by-vfio-on-a-bare-metal-host-pinned-to-the-devices-numa-node-with-hugepages.md)). An operator cannot set a reported label.
   - **Set by the operator.** `naru.garam.sh/site` and any other label.
   - **The region** is the host's region.
4. **The shape filters the hosts.** A host is a candidate when all of these hold:
   - It is admitted, answering and not draining.
   - It is in the shape's region, when the shape names one.
   - It carries every label of the shape.
   - It has the shape's GPUs free on its devices.
   - It has allocatable CPU, memory and disk for the Machine, its volumes included.
   - For a restore, it is the host that keeps the snapshot. A snapshot's host that does not answer or does not fit leaves the restore pending, with that reason.
5. **Machines spread.** Among the candidates, orch takes the host with the largest share of its allocatable memory left after the placement, and breaks a tie by rendezvous hashing of the Machine's `id` over the hosts' IDs.
   - **Why spread.** A host's loss stops fewer Machines, and the first hosts are small nested VMs.
   - **GPU hosts are kept for GPU Machines.** A host with GPUs is a candidate for a Machine without GPUs only when no other host fits. That is Ubicloud's rule, so CPU and memory are not used up beside an idle device.
6. **A Machine that fits nowhere waits, and says why.** Its phase is `pending`. Its reason names the first condition no host met, in the order of item 4. The caller reads which part of its spec or shape it is ("no host fits the memory"), and the operator API reads it with the figures ("no host has 16 GiB of memory free; the most free is 12 GiB"), so a caller learns nothing of the fleet's capacity. Each pass tries again, and each new reason is an event. A use that cannot wait deletes the Machine. Fly.io fails such a request at once, but a use's controller reconciles anyway, and waiting keeps the place in line when a host returns.
7. **A Machine moves only when asked.** There is no automatic rebalancing.
   - **An operator's move.** `POST /v1/machines/{caller}/{name}/move` on orch's operator API (operator, automation, root), with an optional shape that narrows the target, is `naructl machine move`. A target is chosen as for a new Machine, the source host excluded.
   - **Drain.** Draining a host ([orch.md](../orch.md) §Drain) moves its Machines, one at a time per host and two at a time in the fleet. A Machine that fits nowhere else stays, and the drain holds with an event naming it.
   - **A Machine is never moved to balance load**, so a use sees a move only when an operator or a drain asked for one.
8. **Machines are their own reconciler, not a group.** The groups engine ([groups.md](../groups.md)) keeps a count of identical members. A Machine is one record that a use made, and it is placed and moved by `orch/internal/machines`, which shares the allocator `orch/internal/prefixes`, the host client and the allocation records of item 2 with the groups engine.

## Options considered

- **Place from the host's `free` alone, as flaps does.** flaps asks the hosts at each request and needs no account. Orch places in passes and holds stopped Machines and moves that `free` does not show, so it would double-book. Rejected.
- **Bin-packing (the fullest host that fits).** It leaves whole hosts free for large Machines, but it puts more Machines behind each host's loss. The first hosts are two nested VMs where that matters more. Rejected for spreading; reopened if large Machines start to wait.
- **CPU overcommit now.** Workspaces sit idle much of the time. But nothing measures it yet, and a ratio is a policy with a default nobody has chosen. Deferred to an open question.
- **Expressions for requirements and rank, as OpenNebula's.** The shape needs equality of a few labels and a GPU count, so a language would be unused generality. Rejected.
- **The groups engine for Machines.** A group of one per Machine would carry rollout, count and template semantics no Machine has. Rejected.

## Consequences

- A host's operator labels and reported labels are separate fields of the host record, and `GET /v1/hosts` shows both.
- A Machine's resources are reserved on its host while it is stopped. A stopped workspace therefore holds its place, which is what a use expects of a stop. Releasing that place is a delete.
- `naructl machine list|get|move` and `GET /v1/machines` on the operator API show every caller's Machines with their host, which only operators see.

## Signs the decision was wrong

- Machines wait in `pending` while the sum of free capacity across hosts would hold them: fragmentation, which bin-packing or moving to make room would answer.
- Hosts sit mostly idle while their account is full: the case for CPU overcommit, measured.
- Drains hold on Machines that fit nowhere because GPU hosts are kept back.
