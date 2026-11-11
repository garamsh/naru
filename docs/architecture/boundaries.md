# Boundaries

What naru owns, what it leaves to gaon and to mvmd, and the obligations mvmd's deployment puts on it.

In this repository `Machine` names the microVM and `Node` the physical machine. Claims about garamsh/mvmd hold at commit `3b37892f303a4d4dbd8e5360906d0f9c036b562e`, and claims about garamsh/gaon at `cd05f7ed59b62c43279e13d0cb3b3821c93abcc2`; they promise nothing about later versions. Each claim below was read again at those commits when they replaced `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64` and `bf4ed9e716440b33d9b0a701354b05f451727cec`: the mvmd documents the table names, and gaon's `structure.md`, `deployment.md` and ADR 0002.

## Current decisions

### With gaon

- **naru owns the Node:** the node API (apply config, upgrade, reset, status), init as PID 1, the OS image, the installer, and the host and guest kernels.
- **gaon owns** management of machines and clusters, its control plane and its Node Agent, as resources and controllers (gaon ADR 0002, Decision item 1). gaon's documents at the commit above do not mention several clusters, which this line named before.
- **The dependency is one-way, gaon to naru.** naru's node API is the only contract between them. naru does not know gaon's resource API (only the QEMU test tooling reads it, in `tooling/qemu/internal/hostapi`, to check what gaon serves; nothing in the image does), and gaon requires naru's node API client as an external module (`structure.md` §Units).
- **naru has no control plane of its own:** no API server, no store, no authorization system beyond what the node API needs to accept a caller.
- **naru starts and supervises the services its configuration names:** mvmd, gaon's Node Agent and gaon's control plane. The control plane runs as host services and not inside Machines. The image carries gaon's four service binaries (`gaon-node-agent`, `gaon-apiserver`, `gaon-scheduler`, `gaon-controllers`) and etcd, built from the commits [image.md](./image.md) names, and a Node's configuration starts them with argv and files, as gaon's `deployment.md` asks ([ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md)).
- **naru carries gaon's certificates and issues none.** gaon's first milestone has an operator or tooling issue the CA, the API server's pair and one client pair per caller, and naru deliver them as files in the Node's configuration (gaon `deployment.md` §Certificates). naru writes them from `spec.files` under `/run/gaon` and holds no CA; the QEMU tests issue theirs in test tooling.
- **etcd's data is on the data disk,** which is the persistence gaon's `deployment.md` requires of it. The disk is mounted at `/var/lib/naru` with a directory per tenant: `mvmd` (mvmd's `state_dir`), `etcd` and `inputs` ([ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md) item 4).
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

- [ADR 0002](./adr/0002-gaon-depends-on-naru-one-way-through-the-node-api-and-naru-has-no-control-plane.md): why the boundary with gaon sits at the node API, why naru has no control plane, and why init holds no orchestrator logic. It records the consequences and the signs it was wrong.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): why naru owns the guest kernel as well as the host kernel, and the full text of each obligation with the mvmd passage it was read from.
- [ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md): how the gaon services and etcd are carried and started, why certificates are carried and not issued, and where etcd's data lives.

## Open questions

- **The node API's shape.** Its services, calls and versioning are not decided. Until a decision is, this binds nothing.
- **The node API's PKI.** How a caller is authenticated and authorized, and how a first Node with no configuration accepts one, is not decided.
- **The configuration beyond P0.** The fields P0 leaves out (mvmd's prefix, gateway and resolvers, host network settings) and the file's integrity are not decided; they are listed in [config.md](./config.md) §Open questions.
- **The installed Node's partitions.** The partitions of an installed Node and where its system lives are not decided; [image.md](./image.md) §Open questions lists them.
- **The upgrade scheme.** How a Node upgrades and rolls back is not decided.
- **How each mvmd obligation is met.** The capabilities' grant is not decided. The service manager behind the cgroup obligation is init, and what it provides is in [init.md](./init.md).
- **Host network settings for node-to-node connectivity that gaon will need naru's configuration to carry.** The network driver interface takes CSI's shape with gaon as the caller ([discussioncomment-19533869](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19533869), project owner, 2026-11-11), and connectivity between Nodes is host configuration that reaches naru's Node configuration. How Nodes connect (direct routing, WireGuard, overlay) is not decided, so which settings naru has to carry is not decided either.
