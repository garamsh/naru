# ADR 0045: each network of a host is closed by default by an nftables table hostd writes, and WireGuard carries the networks between hosts

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#100 (plan: garamsh/naru#113). It extends [ADR 0028](./0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md) and [ADR 0043](./0043-the-host-api-snapshots-and-resumes-a-microvm-gives-it-a-fence-of-its-own-and-its-types-and-client-are-the-module-lib-hostapi.md) by reference: their bodies are not edited, and what they say holds with this decision added.

## Context

- **Customers share hosts.** A microVM of one customer's network must not reach another's, a host's own services (the host API, the Node API, mvmd) or the management plane.
- **Networks span hosts.** Traffic between two hosts of one network crosses a link nobody trusts, so it is encrypted (garamsh/naru#87).
- **mvmd already confines one microVM.** It accepts a Machine's own addresses and routed prefixes as source only from its tap and applies the Machine's policy. It does not know networks, and it does not name a Machine's tap in its API.
- **The provider carves the prefixes.** Which prefix a network gets is the provider's decision (garamsh/naru#101); hostd enforces what it is told.

## Decision

1. **A network is a name and one prefix,** declared to the host by `PUT /v1/networks/{name}` with an optional API endpoint inside the prefix. Prefixes of two networks on one host do not overlap (409). `GET` and `DELETE` complete the set. Deleting a network detaches its microVMs, which then reach nothing.
2. **A microVM is on a network** when its create names one (`network`). The address mvmd gave it, and its `podPrefix` when it has one, must be inside the network's prefix; else the create is 422 and the microVM is removed. A repeat of the create attaches again. A microVM created with no network, or by a resume, is on none.
3. **One nftables table, `inet naru`, is written by hostd** and replaced whole by one `nft -f` run on every change, so a change is atomic. Its forward chain drops by default and accepts only:
   - the replies of an allowed flow;
   - a network's traffic inside its own prefix, which includes the tunnel's;
   - a network's traffic out of the egress interface, when the host has one;
   - the management plane's traffic to a network's API endpoint, and no other.
4. **Each of these is dropped by a rule of its own, with a counter,** before the accepts: a network to another network's prefix, a network to the management prefix, and a network to the host's addresses (an input chain rule). A packet from a tap whose source is no attached microVM's address or routed prefix is dropped and counted as forged. The tap is found by a name prefix, because mvmd does not report it.
5. **WireGuard between hosts,** one interface. Orch gives the host its peers by `PUT /v1/peers`, the whole list: a public key, an endpoint and the prefixes that peer holds. Those prefixes are the peer's allowed IPs, so the tunnel accepts from a peer only sources it holds and sends it only destinations it holds. A peer dropped from the list is removed from the interface.
6. **Keys are per host,** a file of mode 0600 given by `--tunnel-key`. hostd checks the mode and never reads or prints the file: `wg` reads the key from the path.
7. **The networks are off until `--management-prefix` is given.** Then the calls above work; without it they are 501, and a create naming a network is 501.
8. **Generation is pure,** in `host/internal/network`: the script from the state, and the `wg set` arguments from the state and the interface's current peers. Applying runs `nft` and `wg`; a failure of either leaves the state as it was.
9. **The types and the client** are in `lib/hostapi`.

## Consequences

- The host's state of networks and peers is in memory. After hostd restarts it holds none, and the host forwards nothing for a microVM until orch declares the networks and peers again and repeats each create, which attaches. Closed by default holds meanwhile.
- The tunnel interface must exist with an address before hostd runs; hostd sets its peers and listen port, and does not create it. The image carries neither `wg` nor the kernel's WireGuard option at this ADR's date, so the tunnel needs both added to `image/`.
- A network's microVM on the other host is reached through the tunnel and the forward chain at both ends, so a break of either end closes it.
- The tap name prefix is a flag that must match how mvmd names taps; a mismatch turns the forged-source rule into a rule that matches nothing.
- The counters are in the table; reading them is `nft list table inet naru`.

## Signs the decision was wrong

- Orch cannot re-declare after a restart fast enough: hostd keeps the state in a file.
- mvmd reports a Machine's tap: the rule is per tap, not per prefix of the name.
- A network needs a rule beyond its prefix and its egress: a per-network rule list in the declaration.
- Hosts are many enough that `nft -f` of the whole table on every create is slow: apply incrementally.
