# ADR 0099: a group template lists its members' addresses by number, orch places a member on a host whose Machine prefix holds its address, and mvmd gives it

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#262. It is the sibling of [ADR 0098](./0098-a-kubernetes-node-in-a-microvm-is-one-group-of-one-member-takes-its-pod-prefix-from-the-seeds-meta-data-into-spec-node-and-needs-no-peer-routes.md), which leaves the Node's address open, and it depends on mvmd's ADR 0069 (garamsh/mvmd#817): `CreateMachineRequest.requested_ipv4`.

## Context

- **A Kubernetes cluster needs fixed addresses.** The API server's address is in every kubeconfig, etcd's peers name each other by address, and a Node's serving certificate carries its IP SAN, signed before the Node exists. A microVM's address was whatever mvmd gave after the create ([ADR 0035](./0035-the-host-api-creates-a-microvm-of-an-os-form-from-a-boot-set-with-a-data-disk-and-a-nocloud-seed-and-the-iso-9660-writer-is-a-library-module.md) item 13), so no user-data could name it.
- **Predicting mvmd's allocation is not a contract**, and boot-time TLS is out of scope for v0.1.0-dev.14 (ADR 0098).
- **mvmd now takes a requested address.** `requested_ipv4` is inside the node's Machine prefix; one outside it, or a network, broadcast or unserved-family address, is REJECTED; one another Machine holds, the gateway or a resolver is CONFLICT naming it.
- **Which host can give an address is known to the host.** Each host gives its Machines' addresses from its own Machine prefix (`machinePrefixes` of mvmd's runtime info), so a member's address constrains where the member runs.

## Decision

1. **A template lists addresses.** `template.addresses` is a list of distinct IPv4 addresses in canonical form, for the OS form only (`orchapi.GroupTemplate`, `groups.Template`); a template that lists some and is not the OS form is invalid. Member `<group>-<n>` has `addresses[n-1]`. A template that lists none leaves every address to mvmd, as before.
2. **A member past the list is held.** A group of `count: N` with a longer rollout (`maxSurge`, or a replacement whose number is past the list) needs an address for each number; a member whose number has none is refused by placement with the reason, and the group is held. The operator lists as many addresses as the numbers the group will use.
3. **The hosts report their Machine prefixes.** `GET /v1/host` carries `mvmd.machinePrefixes`, from mvmd's runtime info; orch's host list carries them as `Host.MachinePrefixes`.
4. **Placement is one more stage on the member being placed.** `Place` takes the member's ID and refuses a host none of whose Machine prefixes holds the member's address, with the reason `address <a> is outside the host's Machine prefixes [...]`. A host that reports no prefix (an older host API) is not refused; its create is the check.
5. **orch passes the address through.** `MachineSpec.Address` is sent as `address` in `POST /v1/microvms`; hostd validates it (an IPv4 address in canonical form, else 422 on `address`) and gives it to mvmd as `requested_ipv4`. mvmd's REJECTED is 422 and its CONFLICT is 409, as every other refusal of mvmd; a repeat of the same request by name is the same microVM when the Machine holds the address, and another address under the name is 409.
6. **The seed carries the address mvmd gave**, as before: the network-config is written after the Machine exists.

## Options considered

- **Predict mvmd's allocation.** It is not a contract of mvmd.
- **Read the address after the create and write it into a second user-data.** The seed is attached at the first boot; the certificates are signed before.
- **An address per member in the group's spec rather than the template.** A member's number is the key either way, and the template is already what a rollout compares by hash; a changed address list rolls the members like any template change.

## Consequences

- Changing `addresses` changes the template hash, so every member is replaced; a member keeps its address through a replacement only when the number is the same, which it is not (a replacement is numbered past the existing members). A group that fixes addresses is therefore best run with `maxSurge: 0`, `maxUnavailable: 0` and replaced by hand, as the dev.14 Kubernetes shape (one group of one member per Node) already is.
- A host that reaches the limit of its Machine prefix cannot take another member whose address lies in it; the hold names it.
- hostd's create gains one field and orch's placement one stage; no other call changes.

## Signs the decision was wrong

- A group of several members that must keep addresses through rollouts, which this numbering by member does not give.
- A second family (IPv6) wanted: `requested_ipv6` exists in mvmd and the template lists IPv4 only.
