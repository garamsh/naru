# Boundaries

What naru owns, what it leaves to gaon and to mvmd, and the obligations mvmd's deployment puts on it.

In this repository `Machine` names the microVM and `Node` the physical machine. Claims about garamsh/mvmd hold at commit `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64`, and claims about garamsh/gaon at `bf4ed9e716440b33d9b0a701354b05f451727cec`; they promise nothing about later versions.

## Current decisions

### With gaon

- **naru owns the Node:** the node API (apply config, upgrade, reset, status), init as PID 1, the OS image, the installer, and the host and guest kernels.
- **gaon owns** management of Nodes and clusters, multi-cluster, its control plane and its Node Agent, as resources and controllers.
- **The dependency is one-way, gaon to naru.** naru's node API is the only contract between them. naru does not know gaon's resource API, and gaon requires naru's node API client as an external module (`structure.md` §Units).
- **naru has no control plane of its own:** no API server, no store, no authorization system beyond what the node API needs to accept a caller.
- **naru starts and supervises the services its configuration names:** mvmd, gaon's Node Agent and gaon's control plane. The control plane runs as host services and not inside Machines.
- **No orchestrator logic runs in init.**

### With mvmd

- **naru deploys and supervises mvmd** as a configured service. mvmd keeps no kernel configuration or patches.
- **naru builds and manages the host kernel and the guest kernel.**
- **mvmd's deployment constraints are obligations on naru.** Each row below is one a reviewer looks for in naru's image and configuration when they exist, with its source in mvmd's `docs/architecture/`.

| Obligation | mvmd source |
|---|---|
| Every `mvmd-supervisor` process runs outside mvmd's cgroup. | ADR 0003; `runtime.md` §Fencing |
| mvmd and the supervisor hold the capabilities, limits and kernel features that ADR 0003 (Consequences and Errata), ADR 0015 and ADR 0012 list. | ADR 0003; ADR 0015; ADR 0012 |
| The host kernel has `CONFIG_NF_TABLES` with the netdev and inet families, connection tracking and NAT. | ADR 0012 |
| No other tool flushes the whole nftables ruleset or edits mvmd's two `mvmd` tables. | ADR 0012 |
| IP forwarding is on per family the Node serves; the Node configuration gives a prefix per family, an IPv4 gateway if IPv4 is served, and resolvers. | ADR 0011 |
| mvmd's state directory is on a file system with fs-verity (ext4 with `verity`, f2fs with it, btrfs), never XFS. | ADR 0008 |
| `snapshots/` shares a file system with the Machines' run directories, and root disks stay files in `disks/` of the state directory. | `runtime.md` §State and stability; ADR 0021 |
| The guest kernel meets mvmd's built-in conditions. | `guest.md` §Current decisions |

## Rationale

- [ADR 0002](./adr/0002-gaon-depends-on-naru-one-way-through-the-node-api-and-naru-has-no-control-plane.md): why the boundary with gaon sits at the node API, why naru has no control plane, and why init holds no orchestrator logic. It records the candidates and the signs it was wrong.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): why naru owns the guest kernel as well as the host kernel, and the full text of each obligation with the mvmd passage it was read from.

## Open questions

- **The node API's shape.** Its services, calls and versioning are not decided. Until a decision is, this binds nothing.
- **The node API's PKI.** How a caller is authenticated and authorized, and how a first Node with no configuration accepts one, is not decided.
- **The configuration format.** What a Node's configuration looks like, and how the services it names are described, is not decided.
- **The image layout.** Partitions, the read-only parts and where mvmd's state directory sits against the fs-verity obligation are not decided.
- **The upgrade scheme.** How a Node upgrades and rolls back is not decided.
- **How each mvmd obligation is met.** The service manager behind the cgroup obligation, the capabilities' grant, the kernel configuration and the forwarding setting are not decided.
- **Host network settings for node-to-node connectivity that gaon will need naru's configuration to carry.** The network driver interface takes CSI's shape with gaon as the caller ([discussioncomment-19533869](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19533869), project owner, 2026-11-11), and connectivity between Nodes is host configuration that reaches naru's Node configuration. How Nodes connect (direct routing, WireGuard, overlay) is not decided, so which settings naru has to carry is not decided either.
