# ADR 0119: the proxy role is the operator's grant, and orch tells every host the proxy peers and the mesh pool with its peers, superseding ADR 0112 items 5, 6 and 7 in part

> Status: accepted
> Date: 2026-11-28

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#387, in goal 3, on the review of garamsh/naru#445. It supersedes in part the means of [ADR 0112](./0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md):
- item 5's "each host that reports a proxy", which decides who is sent the table;
- item 6's "the set waited on is the admitted hosts that report a proxy";
- item 7's set of proxy hosts "orch puts with the routes" (`proxies` of `PUT /v1/routes`).

It keeps the outcomes those items state. Only a proxy host takes the table, an address is released only after every proxy has stopped using it, and of the mesh pool only the proxies reach a Machine. The rest of ADR 0112 stands, and its body is not edited. Its errata on the allowance and on each proxy's max age stand beside this record.

## Context

- **Who is a proxy host.** ADR 0112 takes a host's word for it: a host whose `GET /v1/host` reports a proxy is sent the table and is let through to every Machine.
  - The review of #445 found what that word gives a host. The table carries each alias's certificate and key outside the zone, and the proxy mark lets the host's mesh address reach every Machine's service ports on every host.
  - A compromised admitted host can claim both by reporting a proxy. That is an escalation from one host to every tenant's alias keys.
  - The host's `spec.proxy` is the host's own configuration, so orch learns nothing from it that the host could not forge.
- **Where the set of proxy hosts goes.** Item 7 has orch put the set with the routes. Routes go only to proxy hosts, so the host whose Machines the rule guards, one that runs no proxy, would never receive the set.
- **What hostd drops.** Item 7 drops "the rest of `meshPool`". hostd was told only its peers' mesh addresses, so it dropped those alone. A source in the mesh pool that is no listed peer passes, such as a relay's spoke that arrives through a relay peer ([ADR 0113](./0113-hosts-form-a-wireguard-mesh-with-an-address-each-hosts-that-cannot-be-reached-dial-out-to-relays-and-orch-rotates-every-hosts-key-amending-adr-0045-item-5-adr-0048-item-5-and-adr-0049-item-6.md)), and mvmd's per-Machine ingress then admits it to the services' ports.
- **A host that leaves the set while admitted.** A host that stops running a proxy, or now loses its grant, keeps its acknowledgement. Its revision is frozen below every later one, so every later delete waits out its max age, an hour by default, with no end.
- **How long a max age may be.** A host reports its proxy's max age, and orch keeps the longest. Nothing bounded the value: a typo such as `routeMaxAge: 8760h`, or a host that lies, held every delete and every move's clean up in the fleet for as long as the host stayed admitted. A value large enough also wrapped in orch's conversion.
- **The PM's decision on #445's review (2026-11-28).**
  - The proxy role is not self-reported. The operator grants it in the inventory.
  - Orch sends the peer proxy flag, and the route table with the alias keys, only to hosts that hold the grant and report a running proxy.
  - A host that reports a proxy without the grant gets neither, and is an event once per leader.
  - The mesh pool goes with the peers, and is dropped whole but for the proxies.
  - A host that no longer runs a proxy counts as delivered once its last table expired.
  - A reported max age is capped at 24 h.

## Decision

1. **The proxy role is the operator's grant.**
   - The host's inventory record carries `proxy`, false when absent.
   - `POST /v1/hosts/{id}/proxy` grants it, and `DELETE` of the same path revokes it. Both are for an operator and root.
   - Each answers the host as `GET /v1/hosts` lists it, with `proxy`, and is 404 for a host orch does not hold. Granting the role to a host that holds it is not an error.
   - Each is an event, `host.proxy-granted` or `host.proxy-revoked`, whose detail names the caller.
   - `naructl host grant-proxy <id>` and `naructl host revoke-proxy <id>` call them.
   - `spec.proxy` still decides whether a host runs a proxy. The grant decides whether orch trusts that proxy with the role.
2. **A proxy host holds the grant and reports a proxy.**
   - Only a proxy host is sent the table, its alias keys included, and only a proxy host is marked `proxy` on the peers.
   - A host that reports a proxy without the grant is sent neither. The leader logs it and appends `route.proxy-not-granted` once per host in its term.
   - A granted host that reports no proxy is no proxy host. The grant gives nothing to a host that runs no proxy.
3. **The proxy set and the mesh pool go with the peers.**
   - `PUT /v1/peers` carries a `proxy` flag on each peer, refused for a peer with no mesh address, and `meshPool`, the profile's. Orch puts it to every host. `PUT /v1/routes` carries no set.
   - hostd's forward chain accepts to its microVMs' taps the mesh addresses of the proxy peers, then drops and counts every other source in `meshPool`.
   - A host's own proxy reaches its microVMs from the host, by the output path, which the forward chain does not judge.
   - A host that has not yet been told a pool drops its peers' mesh addresses instead. That is the state of a host whose orch predates this record.
4. **The set waited on is the proxy hosts, and a host that leaves it is waited out from its last table.**
   - Each proxy host's acknowledgement keeps the time orch last sent it a table. It is written before the push, as the longest max age is.
   - A host that answers `GET /v1/host` with no proxy, or with one but without the grant, is sent no table. Its acknowledgement is deleted once that time plus its max age plus the release has passed. By then its proxy forwards on no table orch sent it.
   - A host that does not answer stays in the set, waited out from the time recorded for each delete, as before.
5. **A proxy's max age is at most 24 hours.**
   - Init refuses `spec.proxy.routeMaxAge` above 24 h, and hostd refuses `--proxy-route-max-age` above it (`lib/wire.ProxyMaxRouteMaxAge`).
   - Orch takes a reported value that is negative or above 24 h as 24 h, never as the 1 h default it takes for none. The leader logs it and appends `route.max-age-clamped` once per host and value in its term.
   - Orch converts the value only after checking it, so no value wraps.

## Options considered

- **Keep the self-report, and state in `proxy.md` that every admitted host is trusted with every alias key.**
  - Nothing is added.
  - Rejected by the PM. A host's compromise is then every tenant's, and admission was never meant to grant that.
- **A proxy flag at admission, fixed for the host's life.**
  - One field, no new call.
  - Rejected for a grant that can be revoked. A host leaves the proxy role, for example to be drained or rebuilt, without leaving the fleet.
- **Grant from the host's labels.**
  - Labels are already the operator's.
  - Rejected. Labels mean placement elsewhere (ADR 0110), and a label added for placement must not hand out keys by accident.
- **Send the proxy set with the routes, and the routes to every host.**
  - This keeps item 7's means.
  - Rejected. The table carries the alias keys, which no host but a proxy host may hold.
- **Lower a host's max age when it reports a shorter one.**
  - A proxy restarted with a shorter max age may still forward on a table it took under the longer one.
  - Rejected. The longest stays, and the cap is what bounds it.

## Consequences

- An operator who adds a proxy host does two things: configures `spec.proxy` on it and grants it the role. Until both, the host's proxy takes no table and routes no name.
- A revoked host is sent no further table. Its proxy forwards on the one it holds until that table's max age, and a delete waits that out, at most 24 h and the release after the last push.
- A host told the pool drops every mesh source that is not a proxy, relay spokes included, before mvmd's ingress is asked.
- A typo in `routeMaxAge` fails at init instead of holding the fleet's deletes.

## Signs the decision was wrong

- Operators grant the role to every host they admit, so the grant guards nothing. Then admission itself should carry it.
- A proxy host's grant must be checked faster than a pass, for example to cut off a compromised host within seconds. Then revocation needs a push of its own, and the alias keys a rotation.
- A site needs a max age above 24 h to ride out an orch outage. Then the cap, and the release bound with it, should be revisited.
