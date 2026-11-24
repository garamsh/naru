# Proxy

What `naru-proxy` is and how a Machine is reached: the stable names, the aliases uses publish, the routes orch pushes, the forwarding over the host mesh, and the LAN's way in.

None of it is built yet. Each decision below is accepted and waits for the goal-3 issue that builds it. The PR that builds a part states that part here as built.

## Current decisions

- **Stable names** ([ADR 0112](./adr/0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md) item 1).
  - **The forms.** A Machine's default service is `<id>.<zone>`, and its other services are `<service>--<id>.<zone>`. The zone is orch's `--machine-zone`, `m.garam.sh` for the coordinator's fleet.
  - **Why this form.** Each name is one label, so one wildcard certificate covers every name, and no name reveals a caller or a Machine's name.
  - **Moves.** A move never changes the name. The Machine API reports it as the `address`.
- **Services.** A Machine's `services` (`name`, `port`, `protocol` `http` or `tcp`, optional `proxyProtocol: v2`) are all a proxy reaches; the first is the default. `http` carries HTTP/1.1, HTTP/2 and WebSocket. `tcp` is TLS from the client, terminated by SNI, and plain TCP to the Machine.
- **Aliases.** A use publishes `PUT /machines/v1alpha1/aliases/{hostname}` (Machine and service), and the first caller to create a hostname holds it ([ADR 0112](./adr/0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md) item 3).
  - **Inside the zone.** An alias is one label that is not 16 hexadecimal digits and has no `--`.
  - **Outside the zone.** An alias ends in a domain the operator granted the caller (`PUT /v1/callers/{name}` with `aliasDomains`, operator and root) and brings a certificate and key for the hostname.
  - **The key** is never read back: `GET` shows `<set>`. It goes only to the proxies' hosts.
  - **Deleting.** An alias goes with its Machine.
- **`naru-proxy`** is the binary of the unit `proxy/`. It runs as user 6003 on each host whose configuration has `spec.proxy`, with at least two hosts per site.
  - **TLS.** It terminates TLS 1.2 and 1.3 on port 443 with the zone's wildcard certificate (files the operator supplies, which `spec.proxy` names) and each alias's own.
  - **Lookup.** The SNI chooses the certificate. Each `http` request is routed by its own Host or `:authority`, which must be a name the connection's certificate covers, else 421 Misdirected Request, so HTTP/2 connection coalescing under the wildcard works. A `tcp` service is routed by the SNI.
  - **Forwarding.** It forwards to the Machine's private address at the service's port, from its host's mesh address, adding `X-Forwarded-For` and `X-Forwarded-Proto` on `http`.
  - **Credentials.** It holds no credential but the certificates it serves.
- **Routes** ([ADR 0112](./adr/0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md) items 5 and 6).
  - **The table.** Each stable name and alias with the Machine's private address, its services, whether it runs, and an alias's certificate and key. It carries the revision of orch's store.
  - **The push.** The leader puts the whole table, with the set of proxy hosts' mesh addresses, to every host that reports a proxy (`PUT /v1/routes`, provisioner, leader token) on each change and every `--network-interval`. A lower revision is refused, and so is a table sent more than 2 s before it arrives (each push is bounded by a 2 s allowance). The proxy writes the revision it has loaded to a file hostd reads, and hostd answers it; orch records it per proxy host.
  - **On the host.** hostd writes it to its state directory (0640, the proxy's group), and the proxy reloads it.
  - **When orch is away.** A proxy forwards on its last table for `--route-max-age` (default 1 hour) after receiving it, then answers 503. The leased LAN addresses drop within 15 s of orch's loss (ADR 0085 item 5), so routing through them does not survive it; each proxy also listens on its host's own LAN address, and a site whose DNS lists those keeps routing for up to `--route-max-age`. The Machines keep running.
  - **Moves.** A move's commit changes the Machine's entry in the same transaction that rebinds it. A Machine that is not running is answered 503 with `Retry-After: 5`.
  - **No stale route reaches another Machine.** Each entry names the Machine's `id`. Orch deletes a microVM whose address a route named only when every proxy host has acknowledged a revision that no longer names the address, or `--route-max-age` plus 2 s after that revision for a proxy that has not (a removed or abandoned host leaves the set waited on), so an address mvmd reuses is never in a table a proxy still forwards on ([ADR 0112](./adr/0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md) item 6).
- **What reaches a Machine.** Of sources in `meshPool` (hosts' own addresses), hostd's `inet naru` accepts to the host's Machine prefix only the proxy hosts' mesh addresses and drops and counts the rest, so no other host on the mesh reaches a Machine directly; traffic between microVMs of a network across hosts is not touched and stays under the per-network sets; mvmd's ingress, fixed at create, admits the mesh prefix to the services' ports only. Egress is the Machine's `networkPolicy` ([ADR 0112](./adr/0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md) item 7).
- **The LAN's way in is the exposure lease** ([ADR 0112](./adr/0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md) item 8).
  - **The exposure.** An operator declares `PUT /v1/proxy-exposures/{id}` (operator and root): a pool and ports 443, and optionally 80, which redirects.
  - **The lease.** Candidates are hosts with a LAN interface and a proxy that answers its health check. ADR 0085's lease, rendezvous choice and failover apply, with its items 3, 5 and 6 amended for this kind (any pool, the proxy's health as the candidate rule, a redirect to the local proxy).
  - **On the holder.** The holder redirects the ports to its own proxy.
  - **Several addresses.** One exposure per proxy host gives an address on each while both run.
  - **What naru does not run.** The zone's DNS and any path from the internet are the site's.

## Rationale

- [ADR 0112](./adr/0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md): why single-label names with the `id`, why the proxy terminates TLS (and not SNI passthrough), why orch pushes through the host API (and not a proxy credential), why an address is released only on every proxy's acknowledgement, why only proxy hosts reach Machines, why a proxy exposure is a resource of its own, why the proxy runs on hosts (and not as Machines, in garam's cluster, or on every host), and how the lease fits; fly-proxy and E2B's client proxy weighed.
- [ADR 0113](./adr/0113-hosts-form-a-wireguard-mesh-with-an-address-each-hosts-that-cannot-be-reached-dial-out-to-relays-and-orch-rotates-every-hosts-key-amending-adr-0045-item-5-adr-0048-item-5-and-adr-0049-item-6.md): the mesh address the proxy forwards from.

## Open questions

- **Waking a stopped Machine on traffic**, as E2B's client proxy does. A Function service needs it.
- **ACME.** The zone's certificate is a file. DNS-01 issuance needs a DNS provider's credential, which is not granted.
- **A private address kept across moves**, routed to whichever host holds the Machine. It is not taken: uses reach Machines by name.
- **Deltas.** The whole table is pushed. Deltas wait for a table large enough to cost.
