# ADR 0098: a Kubernetes Node in a microVM is one group of one member, takes its Pod prefix from the seed's meta-data into a typed `spec.node`, and needs no peer routes

> Status: superseded by [ADR 0114](./0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md)
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#262. The Node's address in a microVM is a separate decision, made in a sibling ADR with the change that gives a group template its members' addresses.

## Context

- **A group's template has one `userData` for every member** ([orch.md](../orch.md), [groups.md](../groups.md)); orch writes it verbatim and does no templating.
- **A Kubernetes Node's user-data is not generic.** Its name is in the kubelet's and kube-proxy's `--hostname-override`, in the subjects and SANs of the certificates the cluster authority signs, and in its kubeconfigs ([lib/layout/kubernetes.go](../../../lib/layout/kubernetes.go)). A member-generic user-data would need those certificates issued at boot (TLS bootstrapping), which is out of scope for v0.1.0-dev.14. Text placeholders in the user-data cannot fix signed certificates.
- **The Pod prefix is the member's own and orch chooses it** ([groups.md](../groups.md): the first free subnet of `podPrefixBits` of the network's prefix). It is the host that routes it to the microVM (`routed_prefixes`, [host.md](../host.md)), so it cannot be written into a user-data before the member exists. The layout baked it into the CNI configuration and into per-peer `spec.network.routes`.
- **The hosts already route every member's Pod prefix.** mvmd routes a Machine's prefix to it, and orch gives each host's tunnel the Pod prefixes of the members on the other hosts as the peer's allowed IPs (`orch/internal/netrec`); the network's nftables set accepts traffic inside the network. A microVM Node's network-config has a default route through the host's gateway, so a Pod prefix of any member is reached without a route of its own.
- **The NoCloud seed already carries what is the member's alone**: `instance-id` and `local-hostname` (the member's name) in `meta-data`, and the address in `network-config` ([ADR 0035](./0035-the-host-api-creates-a-microvm-of-an-os-form-from-a-boot-set-with-a-data-disk-and-a-nocloud-seed-and-the-iso-9660-writer-is-a-library-module.md)).

## Decision

1. **A Kubernetes Node of the OS form is one group of one member.** The member is `<group>-1`, which the operator knows before the group exists, so the user-data and its certificates use that name (`naructl gen kubernetes-node --name cp1-1`). A group of several members runs a Kubernetes Node only after per-member certificates exist; that is a later change, not part of v0.1.0-dev.14.
2. **hostd writes the member's Pod prefix into the seed's `meta-data`** as `naru-pod-prefix: <prefix>`, when the request has a `podPrefix`. A request with none writes the two keys it wrote before.
3. **init reads it into a typed field.** `ReadSeed` also returns `meta-data` (absent is none). `config.ApplySeedMeta` sets `spec.node.podPrefix` from `naru-pod-prefix`, replacing the user-data's value of the same field, before the configuration is chosen. A meta-data that names no prefix leaves the configuration as it is, and a value that is not an IPv4 prefix with no host bits refuses the boot.
4. **`spec.node` is `{podPrefix, cniConf}`, and init writes the CNI file for every Node.** `cniConf` is an absolute path; when it is set and a Pod prefix is known, init adds to `spec.files` the bridge CNI configuration for that prefix at that path (`config.CNIConflist`, one generator), and removes the entry when no prefix is known, as `spec.host` does for its file. The file therefore follows the field through an apply and a reboot. A configuration that replaces the whole of it must carry `spec.node.podPrefix` (`get config` shows it); a patch keeps it.
5. **`lib/layout` always writes `spec.node`** with `cniConf` and, unless `KubeNode.PodPrefixFromSeed` is set (`naructl gen kubernetes-node --pod-prefix-from-seed`), the baked `podPrefix`. It writes no CNI file of its own. With the flag it names no Pod prefix and no peers, and refuses either beside the flag. The user-data of the two forms differs only in whether it names the prefix; the seed's meta-data overrides it where present. A Node with static addresses keeps its `--peer` routes.
6. **A microVM Node has no peer routes.** The hosts route every member's Pod prefix, and the default route reaches the host.

## Options considered

- **Text placeholders (`${NARU_NAME}`, `${NARU_POD_PREFIX}`) expanded by init.** The certificates still carry the name, so one user-data for N members does not work, and it moves templating from orch into init.
- **Templating by orch.** It is the same problem, and puts text processing in the pipeline's input stage.
- **One user-data per member, with the prefix written by the operator.** The prefix is chosen by orch when the member is planned, after the user-data is written.
- **Keep `--peer` routes for microVM Nodes.** They name addresses and prefixes the operator cannot know, and the host routes them already.

## Consequences

- One user-data serves a group of one member only for the name; the Pod prefix no longer ties the user-data to the plan, so a member that is replaced gets the prefix orch gives it.
- The seed is an interface between hostd and init: `naru-pod-prefix` is a new key of it, read by init only, and `spec.node` is a new section of `naru.yaml`.
- The CNI JSON has one generator, init's; a user-data written by an earlier naructl carries the file in `spec.files` and no `spec.node`, and init writes what it holds.
- A Node whose user-data sets `spec.node.cniConf` and boots with no seed prefix has no CNI file, and its Pods do not start.

## Signs the decision was wrong

- A cluster needs a group of several Kubernetes members before per-member certificates exist.
- A host that does not route another host's members' Pod prefixes (a peer list that omits them) leaves Pods of different Nodes unable to reach each other with no peer routes in the user-data.
- A second per-member value (beyond the name, the address and the Pod prefix) is needed in the user-data, which makes `spec.node` a list of special cases.
