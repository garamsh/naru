# ADR 0059: orch admits a host that boots from an install seed, allocates its Machine prefix and builds its configuration from one fleet profile

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#165, part of plan garamsh/naru#113; it carries decisions of garamsh/aurora#811 comment 6886769198. It builds on [ADR 0058](./0058-a-host-boots-from-a-join-form-that-names-an-orch-and-an-enrollment-key-and-init-applies-the-configuration-hostd-brings-back.md), which made init apply the configuration hostd brings back, and on [ADR 0060](./0060-the-gaon-node-layout-is-the-library-lib-layout-called-by-naructl-and-by-orch-when-it-builds-a-joining-hosts-configuration.md), which made the layout callable by orch.

## Context

- **A host boots from a join form** (ADR 0058) and has nothing else: no CA, no address plan, no configuration. Something has to give it a configuration at admission, and the operator should write the fleet's shared inputs once, not once per host.
- **The form has to reach the host** on a drive the operator can make from a terminal or from the console, with the enrollment key on it and nowhere else.
- **Each host serves its Machines from a /24** of a pool (`spec.host.network`), and two hosts must never hold the same one.
- **Removing a host** had no API: a host that was admitted stayed in the inventory for good.

## Decision

**A host's whole configuration is built by orch at admission from one fleet-wide host profile, the host's own facts and a /24 orch allocates; the host's seed holds the join form and nothing more.**

1. **The install seed is a NoCloud `cidata` ISO 9660 volume.** Its `user-data` is the join form only (`spec.join.orch.url`, `spec.join.orch.caSHA256`, `spec.join.enrollKey`, and an optional `metadata.name`); it has no `network-config`, so the installer asks DHCP. `naructl host install-seed` writes it from a key file (0600), and the console's install-seed page makes the key and the ISO in one request.
2. **`naru-hostd --join` runs the enrollment agent alone.** The host ID is `h-` and the first 16 hex digits of the SHA-256 of the DER of its public key, so a host's name is derived from what it proves. It reports its hardware (processors, memory, disks with size, model and partition count, links with carrier and MAC) and its address, prints the pairing code, and on admission writes the configuration (0600, atomically) and its certificate and exits 0. Init applies the configuration ([ADR 0058](./0058-a-host-boots-from-a-join-form-that-names-an-orch-and-an-enrollment-key-and-init-applies-the-configuration-hostd-brings-back.md)).
3. **The fleet's host profile is one etcd key**, `/naru/orch/profile`, set with `PUT /v1/profile` and read with `GET /v1/profile`, which returns every secret value (a registry password) as `<set>` to every role and keeps the rest of the shape. `PUT` with `<set>` where a password belongs is 422 and changes nothing; `naructl host profile get` prints the redacted form, and `host profile set` refuses such a file before sending it. It holds what every host shares: the guest manifest and Firecracker digest, time servers, registries (with any password), the AWS Roles Anywhere profile, metrics address, host network, storage disks, extensions, the client authority beside orch's, the pool of Machine prefixes and the orch URL and authority digest the host's agent keeps reporting to. `naructl gen host-profile` writes it, 0600 when it carries a password.
4. **The Machine prefix is the lowest free /24 of the pool**, skipping the first when the pool has more than one /24. Each allocation is the key `/naru/orch/allocations/<prefix>`, created only if absent, so two admissions cannot take one prefix: the key is per prefix, not per host, for that reason, and its value records the host. A full pool is 409. A removed host's prefix is freed in the transaction that removes it.
5. **The configuration orch builds is `lib/layout`'s `HostOnly` mode.** It names `metadata.name` as the host ID and a `spec.dataDisk` labelled `NARUDATA` mounted at `/var/lib/naru` (the label the join form's disk has, so apply accepts the change), and carries no `spec.signer`, `spec.orch` or `spec.join`, which the handoff refuses. A host Node has no gaon, so the mode has no CA, no bootstrap token and no Node Agent pair. The host's serving key is a file, `spec.api.keyFile` (`/var/lib/naru/hostd/host.key`), with `certificate` the host's certificate PEM; the AWS credential pair is `/run/naru-hostd/tls.crt` and that key file.
6. **`spec.host.orch.enrollKey` is the sentinel `joined`.** Init requires the field and orch keeps only the key's hash. A host whose certificate expired enrolls again only from a new seed.
7. **A revoked or expired enrollment key stays 403**, whether or not a host used it.
8. **`DELETE /v1/hosts/{id}` (operator and root) removes a host**, frees its prefix, and appends `host.removed`. It does not revoke the host's certificate: the operator does that with `naructl operator revoke`, and `naructl host remove` says so.
9. **The console's install-seed page** makes an enrollment key and returns the ISO as a download. The key is in the file and on no page; each download is an enrollment-key create event by the signed-in person. The pending page shows the hardware beside the pairing code.

## Consequences

- **The operator writes the fleet once** (`gen host-profile`, `host profile set`) and boots as many hosts as there are seeds. A change to the profile applies to hosts admitted after it; hosts already admitted keep the configuration they were given.
- **No role reads a secret back from orch.** An operator who lost a registry password writes it again; orch keeps the stored one for building hosts and shows it to no one.
- **A host's certificate outlives its removal** until it expires or is revoked.
- **There is one definition of a blank disk**, init's, which formats the data disk of a join form. hostd reports what it reads of each disk and calls none blank, so the console cannot show a disk as free that init would not take.
- **The host's certificate has only a common name** as a subject; its SANs are the signer's existing design.

## Signs the decision was wrong

- Hosts need different profiles (per rack, per role), which would say one key is too coarse and the profile belongs per group.
- Re-enrollment after certificate expiry is common enough that a new seed per host is a burden, which would say the sentinel should give way to a key init does not require.
- The first /24 skipped is wanted by someone, which would say the rule belongs in the profile.
