# ADR 0089: orch adds the boot set a group needs to the hosts that place it, from a release store it is configured with

> Status: accepted
> Date: 2026-11-21

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#260, part of goal garamsh/naru#262. It builds on [ADR 0046](./0046-the-host-api-adds-and-removes-a-boot-set-on-a-running-host-through-mvmds-runtime-api-and-a-release-carries-the-node-boot-set-as-a-kernel-an-initramfs-and-a-manifest.md)'s `POST /v1/boot-sets`, [ADR 0042](./0042-the-groups-engine-plans-from-observation-alone-and-replaces-a-stateful-member-only-after-its-fence-has-passed.md)'s engine and [ADR 0080](./0080-a-seed-joined-host-accepts-an-operator-certificate-the-signer-issued-and-takes-its-role-from-the-certificates-organizational-unit.md).

## Context

- **A group placed on a host needs the boot set there.** Placement accepts a host only when the template's `nodeVersion` is among the boot-set names the host's mvmd offers. The only way to add one is `POST /v1/boot-sets` on the host, through `naructl host boot-set add`, one host and one Node version at a time.
- **That is a standing manual step of the fleet, not of the group.** Every new host, and every new Node version, needs it again. A group that never places because the step was missed is visible since #259 and fixed by nobody.
- **Orch already has what the step needs.** It holds a certificate with the orch role on every host it admitted, which the host API treats as the provisioner role ([ADR 0042](./0042-the-groups-engine-plans-from-observation-alone-and-replaces-a-stateful-member-only-after-its-fence-has-passed.md); `host/internal/api/roles.go`), and it knows from its groups which Node versions it needs and which hosts can take each group.
- **What orch lacks is the files.** A release's boot set is `naru-node-<version>.json` and the kernel and initramfs it names ([image.md](../image.md)); orch holds none of them.
- **`nodeVersion` today is the boot-set name.** The manifest's `name` is `naru-node-` and the version with every character outside `a-z`, `0-9` and `-` replaced by `-`, so a version cannot be recovered from a name; and the built-in set of a host is named `naru`.

## Options

- **(a) Orch adds it from a release store it can read.** The operator configures where the release's assets are; orch adds the boot set to the hosts that need it. One step per fleet, once, at configuration. The control hosts need read access to the store.
- **(b) The operator adds it fleet-wide with one command through orch.** `naructl` uploads the two files to orch, which fans them out. The per-host step becomes one per Node version, still by hand, and orch must receive and hold or relay up to 2 GiB per version with nothing to keep it between a request and the hosts that join later; a host admitted after the command lacks the set.
- **(c) It stays per host, by hand.** Nothing changes in code; the procedure is documented. Every fleet pays one step per host and version for ever, and a replacement host is not usable until someone does it.

## Decision

**(a). Orch adds the boot set a group needs to each host that can take the group, from a release store set by `--boot-set-store`.**

1. **The store is the release's assets, read in place.** `--boot-set-store` is an `https://` URL or a directory; the store's layout is the layout of a release: `naru-node-<version>.json` and the files it names beside it. Empty, the feature is off (the trust flags of point 4 are then unused) and the engine behaves as before. The store is configured per member, so a member that becomes leader reads the same.
2. **`nodeVersion` names a release version.** Orch derives the boot-set name from it by the manifest's rule, and a host satisfies the template when its mvmd offers that name. A `nodeVersion` that is exactly a name the host offers (`naru`, the built-in set) is accepted as before and is not fetched. Both are decided in `orch/internal/groups`, so the placement, the host call that creates the microVM and the reconciler use one function.
3. **One reconciler, run by the leader beside the engine.** On each pass, for every group with an OS-form template whose derived name is missing from a host that passes the group's placement filters except the node-version one (admitted, ready, not draining, region and required labels), orch adds the boot set to that host. It adds nothing to a host no group can use, and never removes a boot set.
4. **Nothing reaches a host before it is verified against the release's signature.** Orch checks, in this order: the store's `SHA256SUMS.sigstore.json` is a valid cosign keyless bundle over the store's `SHA256SUMS` ([ADR 0076](./0076-a-tag-publishes-once-and-a-release-is-signed-attested-and-carries-an-sbom-and-the-corresponding-source.md)), signed by the identity the operator configured (default `https://github.com/garamsh/naru/.github/workflows/release.yml@refs/tags/<version>` with the issuer `https://token.actions.githubusercontent.com`, the identity [verify-a-release.md](../../operate/verify-a-release.md) checks by hand); then `naru-node-<version>.json`, the kernel and the initramfs each match their line in `SHA256SUMS`. The manifest is read and checked whole before any file is opened, and each file is hashed as it streams, so a mismatch ends the stream before the host commits it (the host's own digest check is a second, independent check). The trust root is configured on orch, per member, beside the store: `--boot-set-trust-root` (a Sigstore trusted-root file; empty uses the root bundled in the orch binary), `--boot-set-identity` (a certificate identity, or a prefix for a release stream) and `--boot-set-issuer`. Empty identity with a store set is an error at start: a store without an identity to check is not accepted.
5. **The files go from the store to the host as a stream.** After step 4 passes, orch streams the kernel and the initramfs into `POST /v1/boot-sets` with its host-client certificate; nothing is written to orch's disk. Hosts are served one at a time per pass and at most two at once, since a set is hundreds of MiB.
6. **Offline, the verification still runs.** The bundle is verified against the trusted root and the signed-at time proven by the bundle's transparency-log entry, so it needs no network beyond the store. A directory store carries `SHA256SUMS` and `SHA256SUMS.sigstore.json` beside the files (a pinned bundle: the operator copies the release's assets as they are), and a fleet with no route to the internet verifies the same way as one with. Orch never skips the check when the Sigstore services are unreachable; a bundle that cannot be verified offline (no inclusion proof in it) is a failure.
7. **Failure is an event, and the hold stays.** A version the store lacks, a store that does not answer, a host that refuses and a bundle that fails verification, a signer other than the configured identity, a file that does not match `SHA256SUMS` and a digest mismatch each record an event (`bootset.failed`, with the host, the name and the reason, which names the check that failed, repeated at most once per 10 minutes as holds are) and are tried again on the next pass. The group stays held with the node-version reason, which #259 shows to the operator. A success records `bootset.added`.
8. **The per-host command stays.** `naructl host boot-set add` is the way for a host no store serves, and for a fleet without `--boot-set-store`.

## Consequences

- **A group's Node version reaches its hosts without a per-host step,** also for a host admitted later and for a new version, as soon as the release is in the store.
- **Authorization.** No role changes: orch's certificate is already a provisioner on every host it admitted, and `POST /v1/boot-sets` needs that role. What is new is an action orch takes on its own: it puts a kernel and an initramfs on a host that a guest then boots. The store is not trusted: orch verifies the release's signature and `SHA256SUMS` before any byte reaches a host (point 4), so whoever can write to the store, or impersonate its TLS origin, can make a version fail and cannot choose a kernel; what the fleet trusts is the configured identity and trust root, which are orch's flags and an operator's decision. A compromise of the release workflow's identity is out of reach of this check, as it is for `verify-a-release.md`.
- **Orch needs a network path to the store** from each control host; a directory store needs the files on each member.
- **`nodeVersion` changes meaning for a value that is not a name a host offers.** Before it never placed; now it is read as a version. A group that named a boot-set name of another host keeps working.
- **The QEMU test is written and not run** in meet-deadline mode; it applies an OS-form group whose version no admitted host has and shows the member Running with no `boot-set add`.

## Signs the decision was wrong

- Operators want fleet-wide control over which hosts hold which sets, or want sets that no group names added ahead: the reconciler should read a declared list instead of the groups.
- Verification in orch proves too heavy or too brittle (trusted-root rotation breaks fleets, bundles cannot be verified offline): the hosts verify, or a trust root is distributed with the host image.
- Orch's load or bandwidth while streaming shows in the leader's pass time: the host fetches from the store itself.
