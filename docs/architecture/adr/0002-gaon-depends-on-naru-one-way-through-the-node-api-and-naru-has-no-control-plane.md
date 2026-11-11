# ADR 0002: gaon depends on naru one way through the node API, and naru has no control plane

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#2. The direction is the project owner's, recorded in garamsh/discussions Discussion #3 §결정 (decisions 4, 5 and 7) and §결정 5의 이유, and in the summary reply [discussioncomment-19533418](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19533418) on Discussion #2. gaon's side of the same boundary is garamsh/gaon at commit `bf4ed9e716440b33d9b0a701354b05f451727cec`, `docs/architecture/adr/0002-gaon-owns-the-node-agent-and-depends-one-way-on-mvmd-and-naru.md`. In this repository `Machine` names the microVM and `Node` the physical machine.

## Context

- **naru's scope is the Node** (Discussion #3 decision 4): the OS image, init as PID 1, a small node API (apply config, upgrade, reset, status) and the installer. naru builds and manages the host kernel and the guest kernel (decision 6, [ADR 0003](./0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md)).
- **Management of Nodes and clusters, and multi-cluster, are gaon's** (decision 5): gaon's resources and controllers. The owner's first proposal put a fleet manager in naru; that gave two control planes and two multi-cluster layers, and moving it into gaon leaves one API, one CLI, one authorization and one set of certificates (§결정 5의 이유, first bullet).
- **The split inside a Node stays** (§결정 5의 이유, second bullet), for three reasons:
  - PID 1 stopping stops the Node, so no orchestrator logic (watch, lease, wake decisions) runs in it.
  - A first Node has to take configuration before gaon exists, so the Node takes it through an API of its own.
  - gaon has to be testable in a development environment and in CI without naru.
- **naru starts the services its configuration names** (decisions 5 and 7): mvmd, gaon's Node Agent and gaon's control plane. The control plane runs as host services and not inside Machines, which avoids gaon having to start itself on a first Node.
- **gaon's record agrees** at the commit above: its Decision 3 says naru owns the node OS, its installation and upgrade and the guest kernel, does not know gaon's resource API, starts gaon's control-plane services as host services and the Node Agent and mvmd as configured services, and that the node API (apply config, upgrade, reset, status) is naru's to define with its shape undecided. Its Decision 4 makes the dependencies gaon to mvmd and gaon to naru.
- **The network driver interface reaches naru** ([discussioncomment-19533869](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19533869), project owner, 2026-11-11). The interface takes CSI's shape with gaon as the caller, and connectivity between Nodes is host configuration that reaches naru's Node configuration. How Nodes connect (direct routing, WireGuard, overlay) is not decided, so this ADR takes no side on it.
- **Two repositories** (decision 8). Merging them is revisited if changes during the three-node bootstrap phase keep needing both.

## Decision

**naru owns the Node and nothing above it; gaon depends on naru, never the reverse, and naru's node API is the only contract between them.**

1. **naru owns** the node API, init, the OS image, the installer, and the host and guest kernels.
2. **gaon owns** management of Nodes and clusters, multi-cluster, its control plane and its Node Agent. naru runs the control plane as host services and the Node Agent as a configured service.
3. **The dependency is one-way, gaon to naru.** naru does not know gaon's resource API, and no naru unit imports a gaon module. gaon calls the node API and requires naru's node API client as an external module (`docs/architecture/structure.md` §Units).
4. **The node API is the only contract between gaon and naru.** Its shape, its PKI, the configuration format, the image layout and the upgrade scheme are not decided here; each waits under `docs/architecture/boundaries.md` §Open questions.
5. **naru has no control plane of its own:** no API server, no store and no authorization system beyond what the node API needs to accept a caller. What that is waits with the node API's PKI.
6. **No orchestrator logic runs in init.** Init starts and supervises the services the configuration names and holds nothing of gaon's resource model.

## Consequences

- A first Node is configured through the node API before gaon exists, and gaon is tested without naru.
- Anything gaon needs to know about a Node reaches it through the node API, so the API grows by requests from gaon and not by naru learning gaon's resources.
- gaon's control plane and Node Agent are supervised by init like any configured service, so a change to which services run is a change to the Node's configuration.
- naru's design does not depend on how Nodes connect to each other, but its Node configuration may have to carry whatever that decision needs.
- The two projects' records have to move together when the boundary does: gaon's ADR 0002 and this one.

## Signs the decision was wrong

- Changes during the three-node bootstrap phase keep needing both repositories (decision 8's revisit condition).
- The node API grows to carry gaon resources, so naru learns gaon's resource model (gaon's ADR 0002 lists the same sign).
- Init needs watch, lease or wake logic to meet a requirement that the node API cannot meet.
- gaon cannot be tested without a running naru.
- A naru unit needs a store or an API server of its own to do its work.
