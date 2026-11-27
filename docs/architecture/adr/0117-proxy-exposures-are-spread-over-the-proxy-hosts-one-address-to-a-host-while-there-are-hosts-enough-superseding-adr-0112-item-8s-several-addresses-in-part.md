# ADR 0117: Proxy exposures are spread over the proxy hosts, one address to a host while there are hosts enough, superseding ADR 0112 item 8's "Several addresses" in part

> Status: accepted
> Date: 2026-11-25

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#388, in goal garamsh/naru#378. It supersedes the means of [ADR 0112](./0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md) item 8's "Several addresses" bullet, and keeps the outcome that bullet states. The rest of item 8 stands: the exposure, the candidates, the lease and failover, and the redirect on the holder. Its body is not edited.

## Context

- **What ADR 0112 promises.** Item 8, "Several addresses": "An operator who declares one exposure per proxy host gets one address on each host while both run, and both on one host when the other is lost." #388 asks for the same in its fleet test: "With two proxy hosts and two exposures, each address is held by a different host."
- **What the means given can do.** The same item leases each address to one candidate "by rendezvous hashing", which is [ADR 0085](./0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md)'s choice, unchanged. That choice ranks the hosts for each address from a hash of the address and the host's ID, and ignores where the other addresses are. With two addresses and two hosts, both addresses land on one host half the time, and host IDs differ from site to site. The promise holds only by chance.
- **What spreading the addresses buys.** The zone's DNS lists every address, so clients split across them. When both addresses are on one host, that host's proxy carries every connection and the other proxy carries none, while both are healthy.
- **The PM's decision on #388 (2026-11-25).** Proxy exposures are spread across the hosts. The spread is a sequential placement stage of the reconciler, run before its concurrent pass.

## Decision

1. **Placement is a stage of each pass.** Before the reconciler serves any exposure, one sequential stage decides where every exposure that is not being released is to be held.
   - A network's exposure is placed as before, by ADR 0085's rendezvous choice among its candidates.
   - An exposure whose holder is silent, admitted but not answering, stays on that holder within ADR 0085's grace, as before.
   - The concurrent pass then moves and renews each exposure toward its placement, under ADR 0085's lease, confirm-or-deadline and skew, unchanged.
2. **Proxy exposures are spread.** The candidates are ADR 0112 item 8's: hosts that answered, have a LAN interface, and whose proxy is healthy.
   - A proxy exposure kept on a silent holder stays there, and its host, being silent, is no candidate for the rest.
   - The rest are assigned in rounds over every pair of an address and a candidate. A pair's score is the rendezvous hash of the address and the host's ID. In each round the pairs are taken highest score first, numerically by address and then by host ID on a tie, and every candidate takes at most one more address.
   - So with n candidates, n addresses are on n different hosts, the hosts' counts differ by one at most, and no candidate is idle while there are addresses enough.
3. **The placement is a function of the addresses and the candidates alone.** It does not depend on where an address is held now, except on a silent holder. The same set of addresses and candidates gives the same placement on every pass and under every leader, so a new leader moves nothing.

## Options considered

- **Keep pure rendezvous, and amend the promise to "spread by hashing, not guaranteed".** Nothing in the reconciler changes. Rejected: it gives up the outcome item 8 was written for, and half of all two-host sites would run one idle proxy.
- **Prefer the current holder, and spread only new addresses.** Fewer moves when a host returns. Rejected: the placement would depend on the history of passes, so two leaders could disagree. A host that came back would also stay idle until an address was added.
- **Place in address order, each address on its highest-ranked free candidate.** This was this decision's first form. Each address took its host knowing only the addresses before it, so adding an address that sorts low moved those after it whenever their hosts were its own best: over 200 adds of a third address on three hosts (`TestAProxyExposureAddedMovesOnlyTheAddressesItDisplaces`), an existing address moved 113 times. Rejected for the assignment by score, which moved one 96 times over the same adds.
- **A per-host cap on proxy exposures.** It spreads the addresses only when the cap is set to match the hosts, and leaves addresses without a candidate when the cap is too low. Rejected for a rule that needs no setting.

## Consequences

- With two proxy hosts and two proxy exposures, each host holds one while both run. When one host is lost, both addresses are on the other after ADR 0085's grace, lease and skew. When it comes back, it takes one address back, through the same confirm-or-deadline.
- When a candidate is added or removed, the placement of more than the addresses it held can change. With rendezvous alone, only the addresses the host held moved.
- When a proxy exposure is added, the existing addresses that move are a chain of displacements that begins at the new one: an address moves only when an address that scores higher on its host takes it. When one is removed, the addresses it displaced move back. Each move cuts the connections open on its address.
- Each move goes through ADR 0085's release, so no address is held by two hosts.
- The reconciler's pass has one sequential step over the records it already reads, and makes no extra call to a host.
- A network's exposures are untouched by the spread, and proxy exposures take no account of them.

## Signs the decision was wrong

- Sites see proxy addresses move often while hosts or proxy exposures come and go, and the moves cost more than an idle proxy would.
- Proxy hosts differ in capacity, so an even spread is not the right one.
