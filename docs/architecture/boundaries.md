# Boundaries

What naru owns, what it leaves to gaon and to mvmd, and the obligations mvmd's deployment puts on it.

In this repository `Machine` names the microVM and `Node` the physical machine. Claims about garamsh/mvmd hold at commit `191326aca748a0e6e365578016ab87455107164e` (the PM moved the pin there for garamsh/naru#110; they were read at `d189133bba8d842d2bcb20046f39fe03161efeb1` and not read again at the new commit), and claims about garamsh/gaon at `c2b42f91724cae39a4f6824517c278150cf4695f`; they promise nothing about later versions. The gaon claims were read at `c2b42f91724cae39a4f6824517c278150cf4695f` when it replaced `bf4ed9e716440b33d9b0a701354b05f451727cec`: gaon's `structure.md`, `deployment.md` and ADR 0002. The mvmd claims were read again at `d189133bba8d842d2bcb20046f39fe03161efeb1` when it replaced `f08e9130c57c0d71e2bb21ac1deb048563ec3ef8`: the table's mvmd ADRs 0003, 0008, 0011, 0012, 0015 and 0021 have no change between the two commits (`git diff --stat` over them is empty), and `runtime.md` §Fencing, §State and stability and §Network and `guest.md` §Kernel conditions were read at the new commit, where §Network gains the `network.no_masquerade` and runtime-info bullets and an exception for the first in the Enforcement bullet.

## Current decisions

### With gaon

- **naru owns the Node:** the node API (apply config, upgrade, reset, status), init as PID 1, the OS image, the installer, and the host and guest kernels.
- **gaon owns** management of machines and clusters, its control plane and its Node Agent, as resources and controllers (gaon ADR 0002, Decision item 1). gaon's documents at the commit above do not mention several clusters, which this line named before.
- **The dependency is one-way, gaon to naru.** naru's node API is the only contract between them. naru does not know gaon's resource API (only the QEMU test tooling reads it, in `tooling/qemu/internal/hostapi`, to check what gaon serves; nothing in the image does), and gaon requires naru's node API client as an external module (`structure.md` §Units).
- **naru has no control plane of its own:** no API server, no store, no authorization system beyond what the node API needs to accept a caller.
- **naru starts and supervises the services its configuration names:** mvmd, gaon's Node Agent and gaon's control plane. The control plane runs as host services and not inside Machines. The image carries gaon's four service binaries (`gaon-node-agent`, `gaon-apiserver`, `gaon-scheduler`, `gaon-controllers`) and etcd, built from the commits [image.md](./image.md) names, and a Node's configuration starts them with argv and files, as gaon's `deployment.md` asks ([ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md)).
- **naru carries gaon's certificates, and issues them when the configuration carries a CA.** gaon's first milestone has an operator or tooling issue the CA, the API server's pair and one client pair per caller, and naru deliver them as files in the Node's configuration (gaon `deployment.md` §Certificates). naru writes them from `spec.files` under `/run/gaon`; or, when `spec.pki` carries a CA and its key, init issues the pairs on the Node at boot with keys made there and renews them ([ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md)). naru holds no CA of its own: the CA is the configuration's; the QEMU tests and the release's test-only user-data get theirs from test tooling (`tooling/qemu/internal/pki`), the release's from a CA whose key is dropped after signing.
- **A Node carries its registry credentials and trust as configuration.** `spec.registries` gives each registry host a CA and either a username and password or a credential helper name; `spec.aws` gives the IAM Roles Anywhere identity, which the `aws-credentials` service (`aws_signing_helper serve`, with the Node's own agent pair) turns into an instance metadata endpoint on the loopback that `docker-credential-ecr-login` reads through the AWS SDK. mvmd reads the files init writes and runs the helpers on its `PATH`; nothing of this is gaon's, and the Node's trust is the image's pinned bundle plus the CAs the configuration names ([ADR 0026](./adr/0026-a-node-takes-registry-credentials-and-trust-as-configuration-an-ecr-credential-comes-from-iam-roles-anywhere-and-an-agent-pair-without-the-required-organization-is-set-aside.md), [ADR 0029](./adr/0029-aws-credentials-is-a-service-serving-the-instance-metadata-endpoint-to-mvmd.md)).
- **etcd's data is on the data disk,** which is the persistence gaon's `deployment.md` requires of it. The disk is mounted at `/var/lib/naru` with a directory per tenant: `mvmd` (mvmd's `state_dir`), `etcd` and `inputs` ([ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md) item 4).
- **The Node API is decided, and init serves it.** HTTPS with JSON over mutual TLS 1.3 from the package `lib/init/api`, with the roles `naru:admin` and `naru:reader` taken from the client certificate's Organization and the five v1 endpoints (`version`, `services`, a service's `logs`, and `config` to read and to apply). Its authority is the operator's and separate from gaon's CA: `naructl` makes it, no Node carries its key, and a Node with no `spec.api` serves none. [init.md](./init.md) has the endpoints, and [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) the decision.
- **The gaon topology is defined once, in `naructl/internal/layout`:** the services a Node runs and their order, which of them only the control-plane Node runs, and which service signs the agents' requests. A Node's credential mode is one type there (`layout.Mode`). `tooling/qemu` reads the topology from `naructl topology` and keeps no list of its own, so a service added or renamed in the layout is seen by `naru-cluster` and the judge without a change to either ([ADR 0019](./adr/0019-the-gaon-topology-and-a-nodes-credential-mode-are-defined-once-in-naructls-layout-and-the-harness-reads-the-topology-from-naructl-topology.md)).
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
- [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md): why init serves the node API in-process over mutual TLS from an authority of its own, the roles, and the endpoints.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): why naru owns the guest kernel as well as the host kernel, and the full text of each obligation with the mvmd passage it was read from.
- [ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md): how the gaon services and etcd are carried and started, why certificates are carried and not issued (its item 5 is superseded by ADR 0013), and where etcd's data lives.
- [ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md): why init issues certificates at boot from a CA the configuration carries, and renews them.
- [ADR 0019](./adr/0019-the-gaon-topology-and-a-nodes-credential-mode-are-defined-once-in-naructls-layout-and-the-harness-reads-the-topology-from-naructl-topology.md): why the topology and the credential mode have one definition in `naructl`'s layout, and why the harness reads the topology from `naructl topology` and refuses a `naructl` without it.

## Open questions

- **The node API beyond v1.** Reboot, reset and upgrade endpoints, and how a Node with no `spec.api` is reached, are not decided; [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) leaves them to follow-ups.
- **The configuration beyond P0.** The fields P0 leaves out (mvmd's prefix, gateway and resolvers, host network settings) and the file's integrity are not decided; they are listed in [config.md](./config.md) §Open questions.
- **The installed Node's partitions.** The partitions of an installed Node and where its system lives are not decided; [image.md](./image.md) §Open questions lists them.
- **The upgrade scheme.** How a Node upgrades and rolls back is not decided.
- **How each mvmd obligation is met.** The capabilities' grant is not decided. The service manager behind the cgroup obligation is init, and what it provides is in [init.md](./init.md).
- **Host network settings for node-to-node connectivity that gaon will need naru's configuration to carry.** The network driver interface takes CSI's shape with gaon as the caller ([discussioncomment-19533869](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19533869), project owner, 2026-11-11), and connectivity between Nodes is host configuration that reaches naru's Node configuration. How Nodes connect (direct routing, WireGuard, overlay) is not decided, so which settings naru has to carry is not decided either.
