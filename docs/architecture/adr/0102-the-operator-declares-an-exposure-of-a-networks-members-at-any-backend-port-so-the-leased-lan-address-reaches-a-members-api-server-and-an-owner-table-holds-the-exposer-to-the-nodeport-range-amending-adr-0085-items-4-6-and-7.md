# ADR 0102: the operator declares an exposure of a network's members at any backend port so the leased LAN address reaches a member's API server, and an owner table holds the exposer to the NodePort range, amending ADR 0085 items 4, 6 and 7

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#262. It amends [ADR 0085](./0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md) items 4, 6 and 7 by reference, and relies on [ADR 0099](./0099-a-group-template-lists-its-members-addresses-by-number-orch-places-a-member-on-a-host-whose-machine-prefix-holds-its-address-and-mvmd-gives-it.md) for the members' fixed addresses. aurora (garamsh/aurora#832) needs `kubectl` from the LAN to a member's API server.

## Context

- **Nothing routes from the LAN to a network.** A member is a microVM on a network the LAN does not route ([ADR 0045](./0045-each-network-of-a-host-is-closed-by-default-by-an-nftables-table-hostd-writes-and-wireguard-carries-the-networks-between-hosts.md), [ADR 0048](./0048-a-network-is-a-set-of-members-hostd-makes-its-own-tunnel-and-keeps-its-state-in-a-file-and-a-snapshot-keeps-the-disks-it-needs.md)). Its API server listens on port 6443 of the member's address.
- **ADR 0085 already is the path in**: a pool address one host holds by lease, announces by gratuitous ARP and forwards by DNAT. Its target is a Service's NodePort: hostd refuses a backend port outside 30000 to 32767, and the only caller of orch's exposure API is the cluster's own controller.
- **The controller's credential must stay narrow.** It lives in the cluster as a Secret. Letting it name any backend port would let a compromised cluster send the LAN to a Node's kubelet or etcd.
- **The API server's certificate is signed before the Node exists** ([ADR 0098](./0098-a-kubernetes-node-in-a-microvm-is-one-group-of-one-member-takes-its-pod-prefix-from-the-seeds-meta-data-into-spec-node-and-needs-no-peer-routes.md) context), so the address a client dials must be known first. `lib/layout` named only the Node's endpoint and the Service address in the Kubernetes API server's IP SANs; its `ServingAddresses` reached the Node API and the gaon API server only.

## Decision

1. **The backend port is data.** `ExposurePort` carries `backendPort` in place of `nodePort` in `lib/orchapi`, `lib/hostapi` and hostd's state file. There is no alias for the old name: the interface is pre-stable. hostd forwards to the port its lease names and checks only that it is 1 to 65535; it holds no range policy (amends item 6).
2. **An exposure has an owner**, `exposer` or `operator`, the role of the route that made it. The exposer's routes (`/v1/exposures`) read, apply and delete only exposures it owns in its own network. The cloud controller's sweep releases only the exposures it owns, so an operator's exposure is never taken for an orphan. The operator sees every owner's, deletes every owner's (as it did for a revoked exposer's) and applies only its own: an apply over an id the other owner holds is 409 (amends item 4).
3. **One table decides which backend ports an exposure may name, keyed by owner:** exposer 30000 to 32767, the range Kubernetes allocates NodePorts from, and operator 1 to 65535. A spec outside its owner's row is 403. The table is the whole policy: nothing in the pipeline names a port or a service (amends item 7; the strongest thing a stolen exposer credential does is as ADR 0085 item 7 says).
4. **The operator declares an exposure on its own route under the network:** `PUT /v1/networks/{name}/exposures/{id}` (operator role), `GET` of the same (reader role), and the existing `DELETE`. The body is the exposer's `ExposureSpec`, and the call runs the same stage: the pool must list the network, the address is claimed with the record in one transaction, every backend must be the address of a live member of that network, and the reconciler leases the address to one host that runs a backend. `naructl exposure apply -f <file>` and `naructl exposure get <network> <id>` call it. It uses the operator's existing credential, so no credential is added.
5. **The address is declared before the certificate.** The operator applies the exposure with `requestedAddress` from the pool, then gives the same address to `naructl gen kubernetes-node --serving-address` for the control-plane Node. `lib/layout` names `ServingAddresses` in the Kubernetes API server's IP SANs, each once. The members' addresses are fixed by ADR 0099, so the backends are known too. A worker takes no `--serving-address`.
6. **The client of the API server is the announcing host.** The flow is masqueraded for leased addresses (ADR 0085 item 6), so the API server's audit log shows the host's address. Client authentication is by certificate and does not depend on the source address.
7. **A fleet test** declares an exposure of port 6443 on the QEMU bridge of ADR 0085 item 10, reaches it from the LAN client, checks that an exposer is refused port 6443, neither sees nor releases the exposure, and that the operator's delete removes the address. It is written with this decision and is run by the batch.

## Options considered

- **Keep `nodePort` and accept both names.** A second spelling carried for good; the interface changes once, now.
- **A flag on the exposure that marks it declared.** A boolean is a special case every reader must check; the owner is data the table and the routes both key on.
- **Widen the exposer's range.** The credential is in the cluster; the range is its boundary.
- **A new credential for the API server's exposure.** The operator already writes pools and networks and can create any microVM; it needs no second credential for this.
- **Declare the exposure inside `PUT /v1/networks/{name}`.** A network is a prefix and an endpoint that hosts hold; an exposure needs a pool claim, a lease and a record per address. Folding them makes a network apply a claim of addresses.
- **A static DNAT on each host, written by hand.** It has no lease, so two hosts or none hold the address after a failure.
- **Read the address from the API server after it starts and patch the certificate.** The certificate is signed before the Node exists.

## Consequences

- A cluster's API server is reachable on a LAN address that moves with its member's host in under 25 s, as every exposure does (ADR 0085 item 5). A member that moves to another host takes the address with it.
- The operator owns the exposure until it deletes it; nothing in the cluster releases it.
- Exposures made before this change carry no owner: the exposer cannot see them, the operator lists and deletes them, and the controller's apply of the same id is 409 until it has.
- The ccm and `naructl` use `backendPort` in every file and flag; a file naming `nodePort` is refused.
- The API server's audit log records the host's address, not the client's.

## Signs the decision was wrong

- A client of the API server must be identified by its source address: then the operator's exposure takes the PROXY protocol, which the API server does not read, or the access moves to a routed address.
- A third owner (a customer's controller with its own range) is needed: the table gains a row, and an owner that must carry a range per network makes it a pool setting.
