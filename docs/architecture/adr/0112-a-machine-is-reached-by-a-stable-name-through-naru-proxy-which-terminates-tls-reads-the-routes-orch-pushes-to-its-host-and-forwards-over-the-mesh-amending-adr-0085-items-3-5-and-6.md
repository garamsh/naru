# ADR 0112: a Machine is reached by a stable name through naru-proxy, which terminates TLS, reads the routes orch pushes to its host and forwards over the mesh, amending ADR 0085 items 3, 5 and 6

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#373 (goal 3). It decides the routing of #373 §2 and its amendment: "the proxy routes to a Machine's stable name", a use publishes its own names as aliases, and "moving a Machine changes only naru's routing table". It builds on ADR 0108 and ADR 0109, forwards over [ADR 0113](./0113-hosts-form-a-wireguard-mesh-with-an-address-each-hosts-that-cannot-be-reached-dial-out-to-relays-and-orch-rotates-every-hosts-key-amending-adr-0045-item-5-adr-0048-item-5-and-adr-0049-item-6.md)'s mesh, and amends by reference [ADR 0085](./0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md) items 3 (a proxy exposure draws from any pool, which names no network for it), 5 (a proxy exposure's candidate is a host whose proxy answers, not one that runs a member of a network) and 6 (its holder redirects to its own proxy, not DNAT to backends), all in item 8 below. The exposures of ADR 0085 item 4 and [ADR 0102](./0102-the-operator-declares-an-exposure-of-a-networks-members-at-any-backend-port-so-the-leased-lan-address-reaches-a-members-api-server-and-an-owner-table-holds-the-exposer-to-the-nodeport-range-amending-adr-0085-items-4-6-and-7.md) are unchanged.

## Context

- **What changes on a move.** A Machine's private address comes from its host's Machine prefix, so a move changes it (ADR 0109). A use must therefore reach a Machine by a name that a move does not change.
- **How the LAN is reached today.** One host holds a leased LAN address from an operator's pool, announces it, and DNATs it to microVM backends on a network. The lease moves to another host after the holder confirmed the release or the lease and 2 s of skew passed ([orch.md](../orch.md) §Open questions, Exposures; #354, #370).
- **The certificate.** The coordinator approved on 2026-11-24, for #373, as a new credential: a wildcard certificate for the zone `m.garam.sh` that the operator supplies as a file. A use's alias outside the zone brings its own certificate. ACME comes later.
- **Precedents.**
  - **Fly.io's proxy.** fly-proxy "terminate[s] TLS by default", matches connections "to your app’s Machines in the closest region", carries "backhaul between Fly.io servers over WireGuard tunnels", and "gets all its information about apps and Machines and services from corrosion" (docs.fly.io/reference/fly-proxy, read 2026-11-24).
  - **Fly.io's services.** A Machine's `services` define "how Fly Proxy connects requests ... to services running within Machines" (docs.fly.io/machines/api/machines-resource).
  - **E2B.** Its client proxy routes `https://<port>-<sandbox>.<domain>` "at the edge", looks up the sandbox's node in a record the orchestrator writes (`sandbox:routing:{id}`), and forwards to that node (e2b-dev/runtime `README.md` and `docs/ARCHITECTURE.md` at `30a39cf3364197c162576f3823bb7e927f6c82d5`).

## Decision

1. **Every Machine has a stable name in the zone.**
   - **The names.** The Machine's default service is `<id>.m.garam.sh`, where `<id>` is its 16-hex-digit `id` (ADR 0108 item 5). Its other services are `<service>--<id>.m.garam.sh`. The zone is orch's flag `--machine-zone`.
   - **Why one label.** Each name is a single label under the zone, so the one wildcard certificate covers every name.
   - **Why the `id`.** The `id` and not the caller's name is in the name, so a public name reveals neither the caller nor what the use called the Machine.
   - **Service names.** A service name is a DNS label without `--`, which keeps the two forms apart.
   - **Moves.** The name never changes, and it is the `address` the Machine API reports.
2. **A Machine declares the services a proxy may reach.** `services` is a list of `name`, `port` and `protocol`, either `http` or `tcp`. The first is the default service. `http` carries HTTP/1.1, HTTP/2 and WebSocket. `tcp` is TLS from the client, terminated at the proxy by SNI, and plain TCP to the Machine. A `proxyProtocol: v2` on a service writes the client's address first, as hostd's exposures do. Nothing else of a Machine is reachable from outside its host.
3. **A use publishes aliases.** `PUT /machines/v1alpha1/aliases/{hostname}` takes the Machine's name and a service. An alias is unique across the fleet: the first caller to create it holds it.
   - **Inside the zone.** An alias is one label under the zone. It may not be 16 hexadecimal digits, and it may not contain `--`.
   - **Outside the zone.** An alias must end in a domain the operator granted that caller. The operator API gains `PUT /v1/callers/{name}` with `aliasDomains`, for operator and root. The alias also carries a certificate and a key for the hostname, which orch checks and keeps.
   - **The key is never returned.** No read returns it: `GET` shows the certificate's names and expiry, and the key as `<set>`, as the fleet profile shows a password ([orch.md](../orch.md) §Admitting a host that booted from an install seed). It goes only to the proxies' hosts.
   - **Deleting.** A use deletes its alias. Deleting the Machine deletes its aliases.
4. **The proxy is `naru-proxy`, a binary of a new unit, `proxy/`.** It runs as a service on the hosts whose configuration has `spec.proxy`, as user 6003, with at least two hosts per site.
   - **TLS.** It terminates TLS 1.2 and 1.3 on its listener (port 443) with the zone's certificate, from files `spec.proxy` names, and with each alias's own certificate.
   - **Lookup.** The SNI chooses the certificate. On `http`, each request is routed by its own Host header or HTTP/2 `:authority`, which must be a name the connection's certificate covers; another name is answered 421 Misdirected Request, so a client that reuses one HTTP/2 connection for several names under the zone's wildcard (connection coalescing) reaches each, and one that reused a connection beyond its certificate retries on a new one. On `tcp`, the SNI is the route.
   - **Forwarding.** It connects to the Machine's private address at the service's port from its host's mesh address (ADR 0113). It adds `X-Forwarded-For` and `X-Forwarded-Proto` on `http`.
   - **Credentials.** The proxy holds no credential beyond the certificates it serves.
5. **Orch pushes the routes to the proxy hosts.** It does not do so through a credential of the proxy's.
   - **The table.** For each stable name and alias it holds the Machine's private address, its services, whether it is running, and the alias's certificate and key. It carries the revision of orch's store that produced it.
   - **The push.** On every change, and every `--network-interval`, the leader puts the whole table to each host that reports a proxy (`PUT /v1/routes`, provisioner, with the leader token) in the pass that changes it. The host refuses a table with a lower revision. Each push is bounded by an allowance of 2 s, as a lease renewal is (orch.md, exposures): orch abandons a call it has not had answered within it, and the host refuses a table whose `sentAt` (orch's time, in the body) is more than 2 s behind its own clock, so a table sent before a commit is never taken after the commit's allowance. The proxy writes the revision it has loaded to a file hostd reads (and reports it on its health endpoint), and hostd answers each push with that revision; orch records it per proxy host (`/naru/orch/route-acks/<host>`).
   - **On the host.** hostd writes the table to its state directory (mode 0640, group of the proxy's user), and the proxy reloads it on change.
   - **When orch is away.** A proxy keeps forwarding on the last table it received for `--route-max-age` (default 1 hour) after receiving it, then answers 503 to every name until a newer table arrives. Through the leased LAN addresses (item 8), routing does not survive a lost orch: a lease orch does not renew is dropped by its host within 15 s (ADR 0085 item 5), whether or not orch is reachable. Each proxy also listens on its host's own LAN address, which needs no lease, so a site whose DNS lists the proxy hosts' own addresses keeps routing through a lost orch for up to `--route-max-age`, with failover left to clients and DNS. The Machines themselves keep running (ADR 0109 item 1).
6. **A move changes one route, and an address is reused only after every proxy has stopped using it.** At a move's commit (ADR 0109 item 5, step 5), the Machine's entry gets the target's address in the same transaction, and the next push carries it. Each entry names the Machine's `id` beside its address. mvmd gives a deleted microVM's address to a later one, so a proxy still on an older table could otherwise send one Machine's decrypted traffic to another's. Orch therefore deletes a microVM whose address a route named (a move's clean-up, a Machine's delete) only when every proxy host has acknowledged a revision at or above the one that removed that address from the table, or, for a proxy host that has not, once `--route-max-age` plus 2 s has passed since that revision was committed, after which that proxy forwards on no table that named the address: any table it holds was received at most 2 s after it was sent (item 5), and so before the commit plus 2 s. The set waited on is the admitted hosts that report a proxy; a host removed or abandoned leaves it, since it is out of every peer list and so out of the mesh its proxy forwards over. The revision and its commit time are in the store, so a new leader applies the rule.
   - **A Machine that is not running** has its entry marked so. The proxy answers 503 with `Retry-After: 5` without dialling.
   - **Open connections.** Connections open during the stop are closed by the stop.
7. **A Machine admits the proxies and nothing else from the hosts.** Two layers. hostd's `inet naru` accepts traffic to the host's Machine prefix from a source in `meshPool` (a host's own address, ADR 0113) only when the source is the mesh address of a host that runs a proxy, a set orch puts with the routes (`proxies` of `PUT /v1/routes`), and drops and counts the rest of `meshPool`, so another host on the mesh (a user's host, a relay's spoke) cannot reach a Machine directly. Traffic between microVMs, whose sources are Machine and Pod prefixes and never `meshPool`, is not touched by this rule: it stays under the per-network sets, closed by default, that carry a network's members between hosts (ADR 0045 item 5, ADR 0048 items 2 and 3, ADR 0049 item 6). mvmd's per-Machine ingress, fixed at create, admits the mesh prefix to the services' ports only, so even a proxy reaches no other port. Egress is the Machine's `networkPolicy`.
8. **The LAN reaches the proxies through the exposure lease.**
   - **The exposure.** An operator declares a proxy exposure, `PUT /v1/proxy-exposures/{id}` (operator and root), with a pool and the ports (443, and optionally 80, which the proxy answers with a redirect to 443). It has no network and no backends.
   - **Candidates and the lease.** A candidate host is one with a LAN interface whose proxy answers its health check. The leader leases each address to one candidate by rendezvous hashing, and failover is ADR 0085's lease, unchanged.
   - **On the holder.** The holding host forwards the ports to its own proxy in `inet naru` (a redirect to the local listener) and not to microVM backends.
   - **Several addresses.** An operator who declares one exposure per proxy host gets one address on each host while both run, and both on one host when the other is lost. The zone's DNS lists every address.
   - **What naru does not run.** The zone's DNS records (`*.m.garam.sh`) and any path from the internet to the LAN addresses are the site's, not naru's.

## Options considered

- **A proxy exposure as a kind of ADR 0085's exposure** (a backend kind "local proxy" on the existing resource). The existing exposure is keyed by its network (`/naru/orch/exposures/<network>/<id>`), its candidates are the hosts running the network's members, its holder DNATs to listed backends, and its owner table governs backend ports; a proxy exposure has no network, no backends and no backend ports, and its candidate rule is the proxy's health. Every one of those branches would test the kind, which `docs/convention/simplicity.md` §Rules ("Consolidate with judgment") reads as two things in essence. Rejected for a resource of its own that shares the pools, the address-space guard and the lease mechanism.
- **SNI passthrough, with TLS terminated in the Machine.** It needs no certificate at the proxy. But it routes no HTTP by Host, cannot serve an alias without the Machine holding its key, and puts a certificate in every Machine. #373 decided that the proxy terminates TLS. Rejected.
- **Admitting the whole mesh prefix to Machines.** The mesh will carry hosts orch does not run (ADR 0113 item 3), so any of them could reach any Machine's services in plaintext, bypassing the proxy and its TLS. Rejected for the set of proxy hosts.
- **A Machine-aware hop on the target host** (the proxy dials the host, which checks the Machine `id` before forwarding). It would close the stale-route case in the data path itself, at the cost of a second proxy on every host. Rejected for the acknowledged release of item 6, which closes it with no hop; reopened if a release ever has to be forced.
- **The proxy reads orch itself.** It would need a credential of its own, a new role, on every proxy host. Orch's push through the host API reuses the provisioner channel and the leader token that already exist. Rejected.
- **The proxy as Machines.** It would route to Machines from inside the system it routes, and a lost orch would stop its own placement. Rejected.
- **The proxy in garam's cluster (an ingress controller).** naru would then depend on garam's cluster, which #373 §2 rules out. Rejected.
- **A proxy on every host, as Fly.io runs.** Every host would hold the zone's key and a LAN interface. Two per site meet #373's redundancy, and any host can be given `spec.proxy`. Rejected for now.
- **A readable stable name (`<name>.<caller>.<zone>`).** It is two labels, which one wildcard does not cover, and it publishes the caller's and the Machine's names. Rejected.
- **A private address kept across moves** (a routed /32 per Machine that follows it, as mvmd's routed prefixes allow). It would let a use reach a Machine without the proxy, but #373's amendment keeps every layer above naru on names. Not taken now; recorded as an open question.

## Consequences

- garam's Workspace URL is an alias in a domain the operator grants garam, with garam's certificate, or one label in `m.garam.sh`.
- A request in flight when a Machine moves fails. The Machine is down for the move (ADR 0109).
- naru's work:
  - the `proxy/` unit;
  - `spec.proxy` in `lib/layout` and init;
  - `PUT /v1/routes` in the host API and hostd;
  - the proxy exposure in orch and hostd;
  - `PUT /v1/callers/{name}` and the aliases in orch.
- Waking a stopped Machine on traffic, which E2B's client proxy does, is not done. A Function service would need it, and it is an addition to the route's not-running answer.

## Signs the decision was wrong

- Proxies spend more time reloading the whole table than serving, which would argue for pushing deltas.
- Clean-ups wait on proxy acknowledgements long enough to hold addresses a Machine prefix needs.
- The zone's key on a few hosts is the weak point in an incident review, which would argue for ACME with short-lived certificates.
- Uses need to reach a Machine privately, from another Machine, more than through the proxy.

## Note, 2026-11-25

What the decision leaves open, settled where `naru-proxy` was built (garamsh/naru#386, PR garamsh/naru#416):

- **The routes file.** hostd writes it to `/var/lib/naru/proxy-routes/routes.json` (0640, group 6003), in a directory of init's, root's with group 6003, on the data disk. Item 5 says "its state directory", but the proxy can traverse neither hostd's 0700 state directory nor one it cannot read, and must not be able to write the table it forwards on. hostd replaces the file whole, by rename.
- **`receivedAt` and `meshAddress` in the file** (`lib/hostapi.RoutesFile`). `receivedAt` is when hostd took the table, by the host's clock, and the proxy counts item 6's age from it, which holds across a restart of the proxy. A `receivedAt` after the proxy's clock is taken as the load's time, so a clock that steps back does not stretch the bound. `meshAddress` is the address the proxy connects to Machines from.
- **Health** is `GET /healthz` on `127.0.0.1:7480` (item 8's candidacy).
- **The revision file** is `/run/naru-proxy/revision`, the revision the proxy has loaded, which hostd reads and answers orch with (item 5's acknowledgement). The directory is the proxy's, so hostd (garamsh/naru#387) reads it with `O_NOFOLLOW` and a bound on its size.
- **A route that does not check is dropped alone,** by `lib/hostapi`'s `Route.Check`, which orch's alias and hostd's `PUT /v1/routes` apply first. A drop removes a name and never adds an address, so it keeps item 6, and one tenant's bad alias does not stop every other name.
- **Connections are bounded** at 4096 at once and 128 from one address, as hostd's exposure proxy is, with an open-file limit to match, and a client's connection is closed when it has carried nothing in either direction for 5 minutes.
