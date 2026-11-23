# ADR 0097: a Service that sets externalTrafficPolicy Local is sent only to the Nodes that run a ready endpoint of it, from its EndpointSlices, and the client's address comes through PROXY protocol v2, amending ADR 0085 item 8

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#275, part of goal garamsh/naru#262. It amends [ADR 0085](./0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md) item 8 by reference: no body is edited, and what that ADR says holds except where this one says otherwise. The decision was made with the PM in the dispatch of this change (2026-11-23).

## Context

- **ADR 0085 item 8 promises what naru-ccm cannot keep with an event alone.** It says a Service that sets `externalTrafficPolicy: Local` is given the cluster behaviour with an event. The exposure sends to a NodePort on every Node, and with `Local` kube-proxy drops a connection to the NodePort of a Node that runs no endpoint of the Service, so a share of the connections fails.
- **The Service is the user's.** Changing its `externalTrafficPolicy` to `Cluster` would undo what the user asked for and would be fought over by whoever applies the Service again.
- **The host already forwards only to what the lease lists.** A host is a candidate for an exposure only if it runs one of its backends, and its lease lists only the backends it runs ([orch.md](../orch.md)). So the backends the controller stores decide which hosts are leased.
- **Which Node runs an endpoint is in the cluster.** The EndpointSlices of a Service say, for each endpoint, its Node and whether it is ready. A Pod that moves changes neither the Service nor the Nodes, which are what the service controller calls the provider for.

## Decision

1. **Local is supported by sending only to the Nodes that run a ready endpoint of the Service.** naru-ccm chooses the Nodes of an exposure in one stage that takes the Service's policy as data: for `Cluster` (and for no policy) the backends are the Nodes the service controller gave it, as before; for `Local` they are those of them that run a ready endpoint. Nothing in the Service is changed.
2. **The endpoints are the Service's EndpointSlices.** An endpoint counts when it names a Node and its `ready` condition is true or unset (the reading kube-proxy gives an unset condition). A terminating or not-ready endpoint, and one with no Node, counts for none.
3. **The backends are stored again when the slices change.** naru-ccm watches EndpointSlices, and for each changed slice it reads the Service the slice belongs to, and for a Service of type LoadBalancer that sets `Local` it stores the exposure again with the Nodes the service controller last passed to the provider for that Service, through the same selection stage. naru-ccm keeps that list per Service and chooses no Node set of its own, so the two writers of an exposure cannot differ about which Nodes are eligible (the service controller's stable-node-set predicates stay the only ones); before the controller has passed a Service's Nodes, which it does for every Service when it starts, a slice change stores nothing. Changes of one Service within a second are one pass. The add-on's ClusterRole gains `get`, `list` and `watch` on `endpointslices`.
4. **A `Local` Service with no ready endpoint on any Node keeps what it had.** An exposure needs a backend (orch refuses none), so the controller stores nothing and asks again; the exposure that exists keeps its last backends, and its address answers or not as kube-proxy decides until an endpoint is ready.
5. **The event says what naru does.** A Service that sets `Local` receives a Normal event with the reason `ExternalTrafficPolicyLocal`: its traffic goes only to Nodes with a ready endpoint, and the client's address comes with the PROXY protocol annotation, not through `Local`.
6. **The client's address is not preserved by `Local`.** The announcing host forwards by DNAT through the tunnel, and a reply path through the host is the rejected alternative of ADR 0085 item 8. The address still reaches the backend only through the PROXY protocol v2 header a Service asks for with `naru.garam.sh/proxy-protocol: v2`. `Local` here narrows the backends and nothing else.

## Options considered

- **Set the Service to `Cluster`.** It mutates the user's object.
- **The event alone, as ADR 0085 item 8 has it.** Part of the connections fail with no way to avoid it short of editing the Service.
- **Reconcile on the Service and Node events only.** A Pod that moves leaves the exposure sending to a Node that no longer answers until something else changes.
- **Choose the Nodes by the Service's health-check NodePort.** It is the cloud implementations' way of probing, but the probe is made by whoever forwards, which here is the host, and the host forwards by a list from orch; the slices give the same answer without a probe.

## Consequences

- A `Local` Service is reached through the hosts that run a Node with an endpoint, and an address moves to such a host when the endpoint moves, within a second of the slice change plus the lease and skew of ADR 0085.
- naru-ccm holds a watch on EndpointSlices of the cluster and a read of Nodes on each pass.
- A Service that sets `Local` and needs the client's address needs the annotation too.

## Signs the decision was wrong

- Slice churn from a large Service makes passes too many: the pass is bounded by the delay, and a longer one or a per-Service rate is the change.
- A listener needs the client address without a header: then the reply path of ADR 0085 item 8's rejected alternative is decided.
