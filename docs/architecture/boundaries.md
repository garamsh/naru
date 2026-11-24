# Boundaries

What naru owns, what it leaves to mvmd, and the obligations mvmd's deployment puts on it.

The words Node, host, microVM and Machine are those of the [glossary](./glossary.md): a Node is any system naru's init boots, a host is a Node of the host image that serves microVMs, and Machine is mvmd's word for a microVM. Claims about garamsh/mvmd hold at the commit the Go modules pin for mvmd's `lib/runtimeapi`, `b0a6fbe45116301a1a39af97006c7d4a33d8849f` ([ADR 0101](./adr/0101-the-mvmd-the-image-carries-is-the-commit-the-go-modules-pin-and-a-machine-that-does-not-hold-the-requested-address-is-refused.md): the image builds that commit, so the daemon and hostd's client are one version; the PM moved the pin to `b1a60cd649673a6db5fa72ca8cc23f088fd83639` for aurora#811 for aurora#811 from `191326aca748a0e6e365578016ab87455107164e`, which it had for garamsh/naru#110; they were read at `d189133bba8d842d2bcb20046f39fe03161efeb1` and not read again at the new commit); they promise nothing about later versions. The mvmd claims were read again at `d189133bba8d842d2bcb20046f39fe03161efeb1` when it replaced `f08e9130c57c0d71e2bb21ac1deb048563ec3ef8`: the table's mvmd ADRs 0003, 0008, 0011, 0012, 0015 and 0021 have no change between the two commits (`git diff --stat` over them is empty), and `runtime.md` §Fencing, §State and stability and §Network and `guest.md` §Kernel conditions were read at the new commit, where §Network gains the `network.no_masquerade` and runtime-info bullets and an exception for the first in the Enforcement bullet.

## Current decisions

### What naru owns

- **naru owns the Node:** the node API (apply config, upgrade, reset, status), init as PID 1, the OS image, the installer, and the host and guest kernels.
- **naru runs no cluster on its Nodes** ([ADR 0114](./adr/0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md)). No orchestration project's control plane or agent runs on a Node; what places Machines across hosts is orch ([machines.md](./machines.md)).
- **naru starts and supervises the services its configuration names:** mvmd, `naru-hostd` on a host, the registry of the data disk, `aws-credentials` with `spec.aws`, and the daemons of the `naru-orch` extension. They run as host services and not inside Machines.
- **A Node carries its certificates as configuration, and issues them when the configuration carries an authority.** naru writes them from `spec.files`; or, when `spec.pki` carries an authority and its key, init issues the pairs on the Node at boot with keys made there and renews them ([ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md)). The Node API's and the host API's authorities are the operator's (`naructl gen api-ca`, `gen host-ca`), and the fleet's are orch's signer's ([signer.md](./signer.md)).
- **A Node carries its registry credentials and trust as configuration.** `spec.registries` gives each registry host a CA and either a username and password or a credential helper name; `spec.aws` gives the IAM Roles Anywhere identity, which the `aws-credentials` service (`aws_signing_helper serve`, with naru-hostd's serving pair) turns into an instance metadata endpoint on the loopback that `docker-credential-ecr-login` reads through the AWS SDK. mvmd reads the files init writes and runs the helpers on its `PATH`, and the Node's trust is the image's pinned bundle plus the CAs the configuration names ([ADR 0026](./adr/0026-a-node-takes-registry-credentials-and-trust-as-configuration-an-ecr-credential-comes-from-iam-roles-anywhere-and-an-agent-pair-without-the-required-organization-is-set-aside.md), [ADR 0029](./adr/0029-aws-credentials-is-a-service-serving-the-instance-metadata-endpoint-to-mvmd.md)).
- **The data disk is mounted at `/var/lib/naru`** with a directory per tenant: `mvmd` (mvmd's `state_dir`), `inputs` and each daemon's own ([init.md](./init.md)).
- **The Node API is decided, and init serves it.** HTTPS with JSON over mutual TLS 1.3 from the package `lib/init/api`, with the roles `naru:admin` and `naru:reader` taken from the client certificate's Organization and its v1 endpoints (`version`, `services`, a service's `logs`, `time`, `config` to read and to apply, `reboot`, `upgrade`, the `extensions` and the forward to the signer). Its authority is the operator's and separate from every other: `naructl` makes it, no Node carries its key, and a Node with no `spec.api` serves none. [init.md](./init.md) has the endpoints, and [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) the decision.
- **The Node layout is defined once, in `lib/layout`:** the services a Node runs, their order and their files. `naructl` and orch call it, and `tooling/qemu` writes its Nodes by running `naructl` and keeps no list of its own ([ADR 0060](./adr/0060-the-gaon-node-layout-is-the-library-lib-layout-called-by-naructl-and-by-orch-when-it-builds-a-joining-hosts-configuration.md)).
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

- **What Machines across hosts ask of mvmd (decided, not built).** A stream read of a disk no running Machine holds, or of a disk snapshot, and a Create disk from a stream with its size and SHA-256 ([ADR 0109](./adr/0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md) item 8), for the first goal; and, for the GPU goal, device attach in Create Machine with Snapshot refused while a device is attached, the Cloud Hypervisor Driver's VFIO devices, vCPU affinity, hugepages and NUMA with a capability `device-passthrough`, and the device inventory in runtime information ([ADR 0111](./adr/0111-a-gpu-machine-runs-on-the-cloud-hypervisor-driver-with-whole-devices-by-vfio-on-a-bare-metal-host-pinned-to-the-devices-numa-node-with-hugepages.md) item 10). Each is an addition to `v1alpha2` and an issue in garamsh/mvmd.

## Rationale

- [ADR 0114](./adr/0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md): why naru runs no cluster on its Nodes, which supersedes the boundary of ADR 0002, the services of ADR 0012 and the topology of ADR 0019.
- [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md): why init serves the node API in-process over mutual TLS from an authority of its own, the roles, and the endpoints.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): why naru owns the guest kernel as well as the host kernel, and the full text of each obligation with the mvmd passage it was read from.
- [ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md): why init issues certificates at boot from a CA the configuration carries, and renews them.

## Open questions

- **A reset and how a Node with no `spec.api` is reached.** Reboot is decided ([ADR 0022](./adr/0022-a-node-is-rebooted-through-the-node-api-by-post-v1-reboot-or-the-reboot-apply-mode-and-init-answers-before-it-stops.md)) and so is upgrade ([ADR 0025](./adr/0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md)). A call that wipes a Node's store or data disk, and a way into a Node whose configuration has no `spec.api`, are not decided; [target.md](./target.md) §The follow-ups sketches them.
- **The configuration beyond P0.** The fields P0 leaves out (mvmd's prefix, gateway and resolvers, host network settings) and the file's integrity are not decided; they are listed in [config.md](./config.md) §Open questions.
- **The installed Node's partitions.** The partitions of an installed Node and where its system lives are not decided; [image.md](./image.md) §Open questions lists them.
- **How each mvmd obligation is met.** The capabilities' grant is not decided. The service manager behind the cgroup obligation is init, and what it provides is in [init.md](./init.md).
