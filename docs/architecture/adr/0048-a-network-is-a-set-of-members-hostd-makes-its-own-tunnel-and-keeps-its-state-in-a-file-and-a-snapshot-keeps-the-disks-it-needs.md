# ADR 0048: a network is a set of members, hostd makes its own tunnel and keeps its state in a file, and a snapshot keeps the disks it needs

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#100 (plan: garamsh/naru#113). It supersedes [ADR 0045](./0045-each-network-of-a-host-is-closed-by-default-by-an-nftables-table-hostd-writes-and-wireguard-carries-the-networks-between-hosts.md) in its decision 2 (the address must be inside the prefix), decision 4 (a rule per prefix and the host's addresses), decision 6 (the key is a file `wg` reads), the `wg` half of decision 8, and the two Consequences on the state in memory and on the interface that exists before hostd. The rest of ADR 0045 holds. Its body is not edited.

## Context

- **A microVM's address is mvmd's, not the network's.** mvmd assigns an address from the host's Machine prefix, so two networks on one host have microVMs whose addresses are in the same prefix, and a rule per network prefix cannot tell them apart. ADR 0045 required the address inside the network's prefix, which a host that serves two networks cannot satisfy.
- **The state was lost at a restart.** Until orch declared everything again, a restarted host forwarded nothing, and orch had no way to know it had to.
- **The tunnel was a prerequisite.** The interface, its address, `wg` in the image and a key file all had to exist before hostd, and each had a different owner.
- **A snapshot and a delete disagreed** (garamsh/naru#126). hostd's delete removed the disks `<name>.*`, and mvmd restores a snapshot only while the node holds its disks unattached and unchanged, so a snapshot of a microVM with named disks could not be resumed after its source was deleted.

## Decision

1. **A network is a membership set.** Its `prefix` is the Pod and routed space, and a member is a microVM: its address (any address mvmd gave it) and its `podPrefix`, which must be inside the prefix (422 otherwise, before mvmd is called, for a create and for a resume). Prefixes of two networks on a host do not overlap (409), and a prefix that would leave an attached member's `podPrefix` outside is refused (422).
2. **Each network is an nftables set, `net_<name>`,** of interval addresses: its prefix, its local members' addresses and pod prefixes, and its remote members. `PUT /v1/networks/{name}` takes `remoteMembers`, CIDRs of the same network's members on other hosts (IPv4, masked, at most 65536), which orch computes from the other hosts' members and Machine prefixes. A set holds no element inside another, since nft refuses an overlap.
3. **The forward chain decides by set.** It accepts a network's traffic from its set to its set, and drops and counts, each by a rule of its own, a network to another network's set, a network to the management prefix, and a packet from a tap whose source is in no local member's address or pod prefix (forged). An accept to the egress and the management plane's accept to an API endpoint are as in ADR 0045.
4. **The input chain drops a microVM's new connection to the host.** A new connection from a tap to any address of the host is dropped and counted, and so is one from a network's set arriving by the tunnel; established and related are accepted. The host's addresses are no longer a flag: `--host-addrs` is accepted and ignored.
5. **hostd makes the tunnel.** At start, with `--tunnel` set, it creates the WireGuard interface over netlink, generates the private key with `wgtypes.GeneratePrivateKey` into `/var/lib/naru/hostd/wg.key` at mode 0600 when the file is absent (a file with group or other bits is refused), and configures the interface through wgctrl. `PUT /v1/peers` replaces the whole peer list of the interface and routes each peer's prefixes through it, and removes the routes it made that the list no longer holds. The key is never printed or logged, and no API returns it. `GET /v1/tunnel`, open to the reader role, returns `publicKey`, `listenPort` and the host's `machinePrefixes` for orch. hostd does not exec `wg`. The tunnel is behind an interface in `host/internal/network`, so a test supplies a fake.
6. **The state is a file.** After each applied change hostd writes `/var/lib/naru/hostd/networks.json` (networks, members, peers) with a temporary file in the same directory and a rename, at mode 0600. A write that fails restores the previous state and reports the error. At start hostd reads the file and applies it, and applies the empty table when there is none, before the API serves. A corrupt file stops the start.
7. **A resume takes `network`** and `podPrefix` as a create does: validated before mvmd is called, attached after the restore, and the restored Machine stopped and deleted if it cannot be attached. The snapshot's disks stay.
8. **A microVM shows its network.** `GET` of one or all microVMs reports `network` and `podPrefix` from the persisted membership, so orch reads them from the host.
9. **A delete keeps the disks a snapshot needs.** `DELETE /v1/microvms/{name}` deletes the disks of the Machine except those any snapshot lists, and the same holds for the orphan cleanup an absent name triggers. `DELETE /v1/snapshots/{name}` deletes the snapshot and then each of its disks that no other snapshot lists and no Machine holds. A snapshot therefore stays resumable after its source is gone, and its disks go when the last thing that needs them does. This is by reference to mvmd's records, with no state of hostd's.
10. **The types and the client** in `lib/hostapi` carry `RemoteMembers`, `Tunnel`, `Network` on a microVM and on `ResumeRequest`, and `Client.Tunnel`.

## Consequences

- Two networks can share the Machine prefix. What separates them is the set a source and a destination are in, so a member added or removed is one set change in one atomic `nft -f`.
- A host that restarts forwards what it forwarded before, with no help from orch: the table is rebuilt from the file before the API serves. A member whose microVM is gone stays in the file until orch deletes the microVM.
- The private key is a file at `/var/lib/naru/hostd/wg.key`; a host that loses the file makes a new key at its next start, and orch reads the new public key from `GET /v1/tunnel`.
- hostd needs the right to create an interface and the kernel's WireGuard option, which the Node kernel configuration sets (`CONFIG_WIREGUARD=y` in `image/guest/kernels/node/naru.config`). Without `--tunnel`, hostd serves the networks with no tunnel and `GET /v1/tunnel` is 501.
- A snapshot's disks are held on the host for as long as the snapshot is, which costs disk. A disk of a restored Machine keeps its source's name prefix, so the source's delete does not remove it either, and a new microVM with the source's name is refused (409) while a snapshot holds the disk of that name.
- `--host-addrs` is a no-op; a caller that still passes it is not broken.
- The state file holds public keys and prefixes only; the private key is in its own file.

## Signs the decision was wrong

- The set of a network grows past what one `nft -f` applies quickly: apply incrementally.
- orch needs to change a network's members without a whole-table replacement: a member endpoint.
- Disks held for snapshots are mostly unwanted: snapshots carry their disks as a copy, or delete takes an option.
- The state file and mvmd's records disagree after a crash often enough that orch must reconcile: hostd reconciles the file with mvmd's Machines at start.
- One tunnel interface per host is not enough for a host that also runs another WireGuard: an interface per network replaces it.
