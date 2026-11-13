# ADR 0023: spec.network.routes are static IPv4 routes that init installs after forwarding and applies live, and a Node's Machine prefixes are routed to and left unmasqueraded

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#64, part of garamsh/naru#62 (E3) and garamsh/aurora#749. It amends [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) by reference: its body is not edited, and what it says about the classes and the keyed lists holds with this decision added. It rides on [ADR 0008](./0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md), which is how the configuration reaches a Node, and on mvmd's [ADR 0049](https://github.com/garamsh/mvmd/blob/d189133bba8d842d2bcb20046f39fe03161efeb1/docs/architecture/adr/0049-traffic-to-a-configured-no-masquerade-prefix-keeps-the-machines-source-address-and-runtime-info-reports-the-machine-prefixes.md), which adds `network.no_masquerade` and leaves routing between Nodes outside mvmd.

## Context

- **A Node has no route to another Node's Machines.** Each Node's mvmd gives its Machines an IPv4 prefix of its own, and a Node's network-config gives it the default route and the addresses of its links ([ADR 0008](./0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md)). A packet for another Node's prefix leaves by the default route, which does not lead to it.
- **mvmd rewrites the source of what leaves the Node.** A Machine's packet whose destination is outside the Node's Machine prefix has its source replaced by the Node's address (masquerade). The Machine at the other end then sees the Node, not the Machine. mvmd at commit `d189133bba8d842d2bcb20046f39fe03161efeb1` takes `network.no_masquerade`, a list of prefixes, empty by default, whose destinations skip the masquerade; a change of it needs mvmd restarted.
- **mvmd leaves the routing to the Node.** mvmd serves its Machines and installs no route to another Node's prefix: routes to the `no_masquerade` prefixes are the network's and gaon's, not mvmd's (mvmd ADR 0049, `runtime.md` §Network, at commit `d189133bba8d842d2bcb20046f39fe03161efeb1`). The routes belong in the Node's configuration, which init applies.
- **ADR 0018 does not classify a route.** It puts `spec.network` among the reboot-only fields, because the one field in it, `spec.network.forwarding`, is written once at boot. A route is a different kind of change: the kernel takes it at any time, and a route to a new Node is added to a running cluster. ADR 0018 also says a new keyed list is a new decision.
- **Init's boot refuses what it cannot apply, except what is not needed to run.** Init refuses the boot for a network that cannot be applied, and logs and goes on for the time servers ([ADR 0021](./0021-init-keeps-time-from-spec-time-servers-over-sntp.md)), which a Node's services can run without.

## Decision

1. **`spec.network.routes`** is a list of `{to, via}`. `to` is an IPv4 prefix in its canonical form, with no host bits set and not the default route, which the network-config gives. `via` is the next hop, an IPv4 unicast address outside `to`. No `to` is listed twice. A route that breaks one of these refuses the configuration, naming `spec.network.routes[i].to` or `.via` ([config.md](../config.md)). IPv6 routes are not accepted.
2. **A route is installed through netlink** as a static route (protocol `RTPROT_STATIC`) to `to` through `via`, with no link named, so the kernel picks the link that reaches `via`. A route that is already installed is replaced, not an error. Removing a route that is not in the table is not an error either.
3. **Boot order.** Init installs the routes after it turns forwarding on and before it writes `spec.files`: the routes come after the network (step 4) and the Node API, so they never delay either, and a service that starts later finds them in place ([init.md](../init.md)).
4. **A route the kernel refuses does not refuse the boot.** Init logs it at error level, with the route and the kernel's reason, and goes on, as it does for `spec.time`. The route is missing, and the Node's other work is not.
5. **`spec.network.routes` is live, and `spec.network.forwarding` stays reboot-only.** This moves routes out of the reboot-only class of [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) and leaves forwarding in it. A refusal of a `no-reboot` apply therefore names `spec.network.forwarding`, not `spec.network`.
6. **It is a keyed list, keyed by `to`.** A merge patch names the routes it changes by `to`; `$patch: delete` on an element removes the route. A patch that changes only `via` for a `to` replaces that route.
7. **The apply engine changes routes before it writes files.** For the result of a merge, it takes one `delete-route` step for each `to` that is gone and one `add-route` step for each `to` that is new or whose `via` changed, in that order, and before any file write and any service step. The action's target is the prefix. A changed `via` is one `add-route`: the kernel replaces the route, so there is no gap with no route to `to`.
8. **A Node's Machine prefixes are routed to and left unmasqueraded by the layout.** `naructl gen node` takes `--peer <address>=<machine-prefix>` for each other Node of the cluster, repeatable. For each peer it writes a route to the prefix through the address into `spec.network.routes`, and the prefix into mvmd's `network.no_masquerade`. naructl refuses a peer whose address is not an IPv4 address on the Node's own network, or is the Node's own or another peer's, and a peer whose prefix is not a canonical IPv4 prefix, is the default route, or overlaps the Node's network, its own Machine prefix or another peer's: Machine prefixes must be pairwise disjoint for a route to lead to the right Node. With no peer, the user-data has no route and mvmd's configuration has no `no_masquerade`, as before.
9. **mvmd is pinned at `d189133bba8d842d2bcb20046f39fe03161efeb1`**, the commit that adds `network.no_masquerade`, so the image's mvmd takes what item 8 writes.

## Consequences

- A cluster's Nodes route to each other's Machines, and a Machine's source address is its own to the Machines of another Node. Nothing else is done to the traffic: its policy (egress and ingress rules of a Machine) stays mvmd's.
- A route is added to a running Node through the Node API without a reboot. `no_masquerade` is not: it is in mvmd's configuration, a file init writes, and an mvmd that has started keeps what it read. A change of it takes effect when mvmd restarts, which `spec.files` and the service's restart lists already provide for.
- The routes are only as right as `--peer`. naructl writes what it is told: it does not learn another Node's prefix, so adding a Node means regenerating the user-data of the others or applying a route patch to each.
- Routes are static. A Node that is down keeps its routes, and traffic to its Machines is lost until it returns, as with any static route.
- Because a route failure at boot is logged and does not stop the boot, a typo in a next hop that validation accepts (an address on no link) leaves the Node running without that route. The log is the only signal at boot; an apply returns the failure to the caller.
- The boot refusal that named `spec.network` for a reboot-only change now names `spec.network.forwarding`. A client that matched on `spec.network` still matches.

## Signs the decision was wrong

- A Node is regularly found running without a route its configuration lists, and the cause is a failure init logged and went past: the answer is that a route is part of what a Node needs to run, and its failure refuses the boot, as the network's does.
- A cluster needs a route that is not IPv4, not static, or not through one next hop (a route with a metric, a source address, a link, or several next hops): `spec.network.routes` then needs fields, and this decision's `{to, via}` is too small.
- Operators keep `--peer` lists in step by hand and get them wrong, or the prefixes need to change without a Node's user-data changing: the answer is that a Node's peers come from somewhere else, such as gaon's own record of the Nodes.
- `no_masquerade` needs to change without an mvmd restart, or mvmd stops honouring it at a later commit: the pin of item 9 and the layout's file are what then change, and routing between Nodes moves from this decision's static routes to whatever mvmd or the control plane then offers.
