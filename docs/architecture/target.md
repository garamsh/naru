# Target

Which of the eight concepts of a Node operating system naru took as its target are built, and the sketches of the three that are not.

Claims about this repository's code and documents hold at commit `40cc9170e7fe6f91fb8954e0ceea92748a4b0ee6`; they promise nothing about later versions. The concepts are those of the Talos Linux Node operating system, cited by concept and not copied. Goal garamsh/naru#44 decided the eight (decision 1). The Node here is the glossary's ([glossary.md](./glossary.md)).

## Current decisions

- **Five of the eight are built, and their responsibility documents describe them.** Each row below names the ADR that decided it and the document that holds the present state; this file holds neither.
- **The Node API is init's.** It is a package of init, now `lib/init/api`, and not a unit ([structure.md](./structure.md) §Units; [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)).
- **The unit that owns each concept is the unit that owns its state.** Init owns what runs on a Node (services, files, certificates, the store, the API). `naructl` owns what is written for a Node before it runs and what an operator sends it. `image/` and `tooling/disk` own what is on the boot disk. gaon owns nothing here ([boundaries.md](./boundaries.md): naru does not know gaon's resource API).
- **No concept puts orchestrator logic in init** ([ADR 0002](./adr/0002-gaon-depends-on-naru-one-way-through-the-node-api-and-naru-has-no-control-plane.md) item 6). Where a follow-up needs a decision across Nodes (a join, the peers of a cluster), the sketch places it outside init.

## The eight concepts

| Concept | State | Decided in | Present state in |
|---|---|---|---|
| 1. Node API | built | [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) | [init.md](./init.md) |
| 2. Configuration model | built | [ADR 0018](./adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) | [config.md](./config.md), [init.md](./init.md) |
| The reboot endpoint and mode, the step three others needed | built | [ADR 0022](./adr/0022-a-node-is-rebooted-through-the-node-api-by-post-v1-reboot-or-the-reboot-apply-mode-and-init-answers-before-it-stops.md) | [init.md](./init.md) |
| 4. A/B image upgrade with automatic rollback | built | [ADR 0020](./adr/0020-a-node-boots-one-kernel-file-per-slot-of-the-esp-with-the-initramfs-embedded-and-a-trial-slot-is-reached-once-by-bootnext.md), [ADR 0025](./adr/0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md) | [image.md](./image.md), [init.md](./init.md) |
| 5. System extensions | built | [ADR 0055](./adr/0055-orch-and-the-signer-are-delivered-as-a-system-extension-a-squashfs-image-init-mounts-read-only.md) | [init.md](./init.md) |
| 8. Control-plane HA (several etcd members and a virtual address) | built for a set of members fixed when the user-data is written | [ADR 0024](./adr/0024-the-control-plane-is-three-etcd-members-with-tls-on-both-ports-and-a-virtual-address-held-by-naru-vip-behind-a-fence-on-etcds-ports.md) | [config.md](./config.md), [structure.md](./structure.md) |
| 3. Reset, wipe and recovery | sketch | none | below |
| 6. Dynamic Node join | sketch | none | below |
| 7. Discovery and membership | sketch | none | below |

## The three sketches

None is decided; each is a proposal the PM files and an ADR decides.

3. **Reset, wipe and recovery** (a reset call and a maintenance mode). A call that wipes the store, the data disk or both and reboots; and a mode in which a Node with no valid configuration still serves a limited API, which today it cannot, since init stops before any service starts. Owner: init. Depends on: the reboot endpoint, the store. Supersedes: the part of [init.md](./init.md)'s "A refusal stops every service" that leaves a refused Node with no way in, if a maintenance mode serves the API there. On a control-plane Node, a wipe needs etcd's member removed first, which no built piece does.
6. **Dynamic Node join** (tokens issued and revoked through the API). The first form needs no new endpoint: a token is a line of `/run/gaon/bootstrap-tokens`, a `spec.files` entry of the control-plane Node with `restart: [gaon-apiserver]`, so `naructl` can add or revoke one with a patch to that Node, which names the `spec.files` entry by `path`. A dedicated token endpoint waits for a reason this form cannot meet. Owner: `naructl` (the token commands, the layout), init (the apply of ADR 0018). Depends on: the Node API and the configuration model, both built. Supersedes: [config.md](./config.md)'s procedure to add a Node ("apply that new user-data … the control-plane Node restarts with it") and [ADR 0016](./adr/0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md)'s consequence "Adding a Node needs the control-plane Node's user-data again".
7. **Discovery and membership.** Nodes know each other: the set of peers of a cluster, for the etcd peers of a control plane whose members change and for a call forwarded through one Node. The first form is a static list the operator's tool writes, `naructl` as the source; a Node-to-Node protocol waits for a need a list cannot meet. It cannot read gaon's Node resources (ADR 0002 item 3). Owner: `naructl` (the list), init (reading it, and forwarding if it comes to that). Depends on: the Node API; forwarding also reopens ADR 0017 item 1. Supersedes: nothing, for the static list. A forwarding init would supersede ADR 0017 item 1.

## Order of the sketches

Each line gives what it needs first.

1. **Dynamic join.** Needs nothing unbuilt; it does not wait for 2.
2. **Reset, wipe and recovery.** Needs the reboot endpoint, which is built.
3. **Discovery and membership.** Needs the Node API, which is built; changing the members of a running control plane needs it, and needs 1 and 2.

## Rationale

- [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md): why init serves the API, why a separate authority, and the signs it was wrong.
- [ADR 0018](./adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md): why a base with revisions, the boot choice, and the signs it was wrong.
- [ADR 0002](./adr/0002-gaon-depends-on-naru-one-way-through-the-node-api-and-naru-has-no-control-plane.md): why no concept puts orchestrator logic in init, and why naru does not read gaon's resources.
- The order follows what each sketch cannot work without.

## Open questions

- **A maintenance mode's trust.** A Node with no valid configuration has no `spec.api` and so no authority to trust; how it accepts an operator is not decided.
- **How an upgrade bundle is signed.** The call carries the bundle, checked against the release's SHA-256 ([ADR 0025](./adr/0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md)); a signature over it ([image.md](./image.md) §Open questions, Signing) is not decided.
- **Whether a Node API client library under `lib/` is built.** gaon would require it as an external module ([structure.md](./structure.md) §Units); nothing asks for it yet.
