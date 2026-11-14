# ADR 0049: orch keeps the networks and allocates each member's Pod prefix by compare-and-swap, and the leader tells every host its networks, their members and its peers

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#100 and garamsh/naru#118 (plan: garamsh/naru#113). It supersedes [ADR 0045](./0045-each-network-of-a-host-is-closed-by-default-by-an-nftables-table-hostd-writes-and-wireguard-carries-the-networks-between-hosts.md) decisions 2 and 4 in part, and extends [ADR 0042](./0042-the-groups-engine-plans-from-observation-alone-and-replaces-a-stateful-member-only-after-its-fence-has-passed.md) by reference: their bodies are not edited, and what they say holds with this decision added.

## Context

- **A network is more than a prefix.** mvmd gives each microVM an address out of its host's Machine prefix, and naru cannot choose it, so two networks cannot each hold their microVMs' addresses inside a prefix of their own. ADR 0045 decision 2 (an address inside the network's prefix) and decision 4 (the forward chain keyed on prefixes) assumed it could. A network is a prefix, for the Pod and routed space, plus the members that are on it: the address and Pod prefix of each microVM attached to it, on every host.
- **Only orch sees every host.** A host knows its own microVMs. The members of a network on the other hosts, and the other hosts as peers of the tunnel, are known to orch alone, so orch has to tell each host.
- **A Pod prefix must be held by one member.** A member that takes a prefix another member of the network holds would receive that member's traffic. Two groups on one network, two leaders during a failover, and a replacement created before the old member is gone are each a way to hand a prefix out twice.
- **The group template has no network.** `RootDiskBytes` and the OS form's seed were constants of the host client.

## Decision

1. **Orch keeps the networks,** under `/naru/orch/networks/<name>` in etcd: `PUT|GET|DELETE /v1/networks[/{name}]` and `GET /v1/networks`, with `{prefix, apiEndpoint?}`. Writes need the operator role and reads the reader role. The name is a DNS label. The prefix is IPv4 with its host bits clear, and the endpoint, when given, is an address inside it. Every applied or deleted network is an event.
2. **A network a group uses is held.** A `DELETE` of it, or an apply that changes its prefix, is 409 while any group's template names it, since the members' Pod prefixes were cut from the prefix.
3. **The group template names a network** with `network`, and gives each member a Pod prefix of `podPrefixBits` bits cut from the network's prefix. An apply that names a network orch does not keep, or a prefix length not longer than the network's, is 400. The template's network policy moves to the key `policy`, so that `network` is the virtual network.
4. **The planner picks the Pod prefix,** the first subnet of the requested length that overlaps no prefix held by a member of that network that is not gone, in any group and earlier in the same pass. A network that is not declared, or has no subnet left, holds the group: a hold event, no create.
5. **A claim makes the prefix one member's.** Creating a member record that carries a Pod prefix is one etcd transaction with a claim key `/naru/orch/pod-prefix-claims/<network>/<address and length>`, which must not exist yet; its value is the member's ID. A lost claim is a conflict, the pass stops that action, and the next pass plans from what the store holds. Writing the member as gone deletes the claim only when its value is that member's ID, so a stale write of a gone member does not free a prefix another member holds. The member's ID is never reused, so a claim names one member for good.
6. **The leader runs a network reconciler** beside the groups engine, one pass per `--network-interval` (default 10 s), in the package `orch/internal/netrec` behind a seam. For every admitted host that answers `GET /v1/tunnel` and `GET /v1/microvms`, it:
   - declares every network with `PUT /v1/networks/{name}`: the prefix, the endpoint, and the `remoteMembers`, which are the addresses (as `/32`) and the Pod prefixes of the network's microVMs on the other hosts;
   - puts the whole list of peers with `PUT /v1/peers`: every other host that answered, with its tunnel's public key, its endpoint at the address its enrollment recorded and the tunnel's port, and as prefixes its Machine prefixes and the Pod prefixes of its members.
   A host that does not answer is skipped for the pass and logged, is a peer of none, and the other hosts still converge. The reconciler keeps nothing between passes, so it also makes a host whole after the host restarts or orch is restored.
7. **A microVM's network comes from the host.** The host API reports each microVM's `network` and `podPrefix` from the host's persisted membership, so the reconciler and `GET /v1/microvms` see the microVMs no group made. `GET /v1/microvms` reports each microVM's image or boot set and its network.
8. **The host client sets what the template says:** `Network`, `PodPrefix`, `RootDiskBytes` (an image-form template needs it above zero, else the create is refused) and the OS form's `Seed`, which is the template's `userData` or, when empty, `#cloud-config`.

## Consequences

- The prefix is freed when the member's stored phase becomes gone, not when its microVM is actually removed: a member whose host is away is gone for the engine once the engine writes it so, while the host may still hold the microVM, and a new member may take the same prefix. The host then holds two attachments of one prefix until it is back and removes the first.
- The engine may create a microVM before the reconciler has declared the network on that host. A create naming a network the host does not hold fails, and the next pass makes it, so the window is one reconciler interval after a network is first applied or a host is admitted.
- The reconciler reads each host's microVMs once a pass beside the groups engine's own reading, so each host is asked twice as often.
- A stored group template written before this decision with the network policy under `network` does not decode. The groups API is not released, so nothing is migrated.
- Networks are IPv4 only.
- Moving the policy to `policy` changes the YAML and JSON key of a group's network policy, for naructl and the console.

## Signs the decision was wrong

- A prefix handed to a second member in practice, because the window between the stored gone phase and the host's removal is hit: free the claim only after the host reports the microVM absent.
- Hosts are many enough that the reconciler's reads and writes of every host each interval are slow: send only what changed, or a pass per host.
- A network needs per-network rules beyond its members: a rule list in the declaration, as ADR 0045 allows for.
- Declaring the network on a host after the engine's create costs a visible delay: the engine declares the network on the host before it creates.
