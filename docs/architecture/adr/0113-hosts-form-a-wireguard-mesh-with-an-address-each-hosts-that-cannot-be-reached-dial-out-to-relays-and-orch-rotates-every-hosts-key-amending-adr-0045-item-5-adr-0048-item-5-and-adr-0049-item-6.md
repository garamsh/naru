# ADR 0113: hosts form a WireGuard mesh with an address each, hosts that cannot be reached dial out to relays, and orch rotates every host's key, amending ADR 0045 item 5, ADR 0048 item 5 and ADR 0049 item 6

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#373 (goal 3). It decides the host mesh of #373 §2: "WireGuard between hosts and to the proxy", with bare metal and user infrastructure joining later "by enrollment (host-initiated, outbound)". It amends by reference ADR 0045 item 5 (a peer's allowed IPs are its prefixes: they now include its mesh address), ADR 0048 item 5 (the tunnel hostd makes now carries an address and can be rotated) and ADR 0049 item 6 (the peers orch declares carry mesh addresses, and an unreachable host's peers are its relays). It extends the tunnel of [ADR 0045](./0045-each-network-of-a-host-is-closed-by-default-by-an-nftables-table-hostd-writes-and-wireguard-carries-the-networks-between-hosts.md) and [ADR 0048](./0048-a-network-is-a-set-of-members-hostd-makes-its-own-tunnel-and-keeps-its-state-in-a-file-and-a-snapshot-keeps-the-disks-it-needs.md), the peers orch declares under [ADR 0049](./0049-orch-keeps-the-networks-and-allocates-each-members-pod-prefix-by-compare-and-swap-and-the-leader-tells-every-host-its-networks-members-and-peers.md), and the allocator of [ADR 0066](./0066-orch-reserves-a-machine-prefix-before-it-asks-the-signer-and-commits-it-at-admission-keeps-no-credential-in-a-host-record-and-allocates-every-prefix-through-one-function.md) by reference.

## Context

- **What the tunnel carries.** With `--tunnel`, hostd makes a WireGuard interface and its key, which it never returns or logs. `GET /v1/tunnel` shows the public key, the port and the Machine prefixes. Orch's network reconciler puts every other answering host as a peer (`PUT /v1/peers`), at the host's address with the tunnel's port and with its Machine prefixes and Pod prefixes as allowed IPs ([host.md](../host.md) §The tunnel; [orch.md](../orch.md) §Networks). The tunnel carries microVM traffic between hosts. It has no address of its own, so a host's own traffic (a proxy, a disk copy) leaves by the LAN with a source no peer allows.
- **Every peer is dialled at its address.** That holds for hosts on one LAN and fails for a host behind NAT or a firewall that admits no inbound, which is the case for user infrastructure and for a bare-metal host at another site.
- **Enrollment already goes outward.** A host sends its enrollment key to orch's URL and polls until it is admitted ([orch.md](../orch.md) §Enrollment). It never needs to be dialled to join.
- **Keys are never rotated.** A host's tunnel key is made at its first start and kept.
- **Precedents.** Fly.io carries "backhaul between Fly.io servers over WireGuard tunnels" (docs.fly.io/reference/fly-proxy, read 2026-11-24). Ubicloud encrypts the private network between its hosts with IPsec (ubicloud/ubicloud `README.md` at `8417580759fd085fa20031dde0ba1f6ae1f3152f`). #373 §3 names the outbound-only pattern for things a user runs: "like GitHub self-hosted runners, Rancher agents and the Terraform Cloud agent".

## Decision

1. **Every host has a mesh address.**
   - **Allocation.** The fleet profile gains `meshPool`, an IPv4 prefix kept apart from the Machine pool and every network. At admission orch gives each host one /32 from it, through the one allocator `orch/internal/prefixes`, by ADR 0066's reserve-then-commit. A host admitted before this change is given one by the leader's recorder on its first pass, as ADR 0103 records Machine prefixes.
   - **On the host.** hostd puts the address on the tunnel interface. `GET /v1/tunnel` shows it.
   - **On the peers.** Orch's `PUT /v1/peers` lists each peer's mesh address beside its prefixes.
   - **Host-to-host traffic.** A host's own traffic to another host goes from and to mesh addresses, inside WireGuard: the proxy's forwarding (ADR 0112) and the copy of a volume (ADR 0109 item 8).
2. **The proxies are on the mesh because they run on hosts.** A proxy dials a Machine from its host's mesh address, and that source is the one ADR 0112's ingress rule admits.
3. **Hosts that can be dialled form a full mesh, and the others dial out to relays.**
   - **Unreachable hosts.** A host whose configuration sets `spec.host.mesh.reachable: false` (one behind NAT, or in a user's network) is dialled by no one. It peers only with the relays, and sends a keepalive every 25 s so its NAT keeps the path open.
   - **Relays.** A relay is a reachable host that the operator labels `naru.garam.sh/relay: "true"` and that has a public endpoint (`spec.host.mesh.endpoint`). It forwards between the hosts it carries. A spoke's peer entry for its relay lists the prefixes and mesh addresses of every host the spoke reaches through it.
   - **Endpoints.** A relay learns a spoke's endpoint from the spoke's handshakes, so none is configured for it.
   - **A spoke cannot pose as another host.** A relay's peer entry for a spoke allows only the spoke's own mesh address and prefixes as sources (WireGuard's allowed IPs), so a spoke's packet from a proxy's mesh address is dropped, and a Machine admits only the proxies' addresses (ADR 0112 item 7).
   - **Placement of relays.** Orch declares at least two relays to each unreachable host when two exist, chosen by rendezvous hashing.
4. **A host outside the site joins outward, then is reached over the mesh.**
   - **Joining.** It enrolls as any host does: its join form names orch's URL, which the site publishes to where the host is.
   - **Admission.** The configuration orch builds at admission carries the host's mesh address and its relays as its first peers, so the mesh is up before orch first calls it.
   - **Afterwards.** Orch reaches such a host's API at its mesh address, and the host sends its heartbeat to orch's mesh address. A host on the site's LAN keeps being reached at its LAN address.
   - **Bare metal and user infrastructure** join this way, by the same enrollment and admission. They are told apart only by their labels and reported facts (ADR 0110 item 3).
5. **Orch rotates every host's tunnel key.**
   - **The call.** `POST /v1/tunnel/rotate` (provisioner, with the leader token) makes hostd generate a new key, write it whole in place of the old (`lib/atomicfile`), apply it, and answer the new public key.
   - **When.** The leader rotates a host whose key is older than `--mesh-key-max-age` (default 30 days). It rotates one host per pass, and pushes the new key to every peer in the same pass. Traffic between that host and its peers stops for at most that pass.
   - **On demand.** An operator rotates a host at once with `naructl host rotate-tunnel`.
   - **Removal.** A host that is removed, or whose certificate is revoked, leaves every peer list in the next pass, as it does now.
6. **The first goal builds items 1 and 2.** Its hosts are two nested hosts on one LAN. Items 3, 4 and 5 (relays, hosts outside the site, and rotation) are designed here and built in the later goal that brings in bare-metal and user hosts, where a key travels beyond the site.

## Options considered

- **IPsec between hosts, as Ubicloud runs.** WireGuard is already hostd's tunnel and orch's peer model, and it needs no key exchange daemon. Rejected.
- **A tunnel address on the LAN address, and no mesh address.** A host's own traffic would leave unencrypted and outside the peers' allowed IPs. Rejected.
- **Every host dialled, with NAT traversal by port mapping.** A user's network may forbid inbound entirely, and #373 asks for host-initiated, outbound joining. Rejected for relays.
- **A separate overlay (Tailscale, Nebula).** It would be a second key system and a second control plane beside orch's peers. Rejected.
- **Pre-shared keys on every peer pair.** They add a secret per pair for protection the rotation already bounds. Not taken now.

## Consequences

- A host's own connections to another host are encrypted and attributable to the host's mesh address.
- A relay carries its spokes' traffic, so its capacity bounds theirs. The later goal measures it.
- The profile gains `meshPool`, and `spec.host` gains `mesh` (`reachable`, `endpoint`).

## Signs the decision was wrong

- Rotations break host-to-host traffic for longer than a pass, which would argue for keeping two keys and two interfaces during a rotation.
- Spokes depend on a relay that is down more often than their own uptime, which would argue for direct paths by hole punching.
- Traffic between spokes through relays dominates, which would argue for placing a use's Machines on one side.
