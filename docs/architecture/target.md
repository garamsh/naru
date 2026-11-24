# Target

Which of the eight concepts of a Node operating system naru took as its target are built, which two were removed, and the sketch of the one that is not.

Claims about this repository's code and documents hold at commit `40cc9170e7fe6f91fb8954e0ceea92748a4b0ee6`; they promise nothing about later versions. The concepts are those of the Talos Linux Node operating system, cited by concept and not copied. Goal garamsh/naru#44 decided the eight (decision 1). The Node here is the glossary's ([glossary.md](./glossary.md)).

## Current decisions

- **Five of the eight are built, and their responsibility documents describe them.** Each row below names the ADR that decided it and the document that holds the present state; this file holds neither.
- **The Node API is init's.** It is a package of init, now `lib/init/api`, and not a unit ([structure.md](./structure.md) §Units; [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)).
- **The unit that owns each concept is the unit that owns its state.** Init owns what runs on a Node (services, files, certificates, the store, the API). `naructl` owns what is written for a Node before it runs and what an operator sends it. `image/` and `tooling/disk` own what is on the boot disk. Orch owns what is placed across hosts ([orch.md](./orch.md)).
- **No concept puts orchestrator logic in init** ([ADR 0002](./adr/0002-gaon-depends-on-naru-one-way-through-the-node-api-and-naru-has-no-control-plane.md) item 6). Where a follow-up needs a decision across Nodes (a join, the peers of a cluster), the sketch places it outside init.

## The eight concepts

| Concept | State | Decided in | Present state in |
|---|---|---|---|
| 1. Node API | built | [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) | [init.md](./init.md) |
| 2. Configuration model | built | [ADR 0018](./adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) | [config.md](./config.md), [init.md](./init.md) |
| The reboot endpoint and mode, the step three others needed | built | [ADR 0022](./adr/0022-a-node-is-rebooted-through-the-node-api-by-post-v1-reboot-or-the-reboot-apply-mode-and-init-answers-before-it-stops.md) | [init.md](./init.md) |
| 3. Reset, wipe and recovery (a host's; a Node is replaced, not reset) | built | [ADR 0082](./adr/0082-a-host-resets-its-store-its-data-disk-or-both-through-post-v1-reset-at-its-next-boot-and-a-refused-host-serves-maintenance-mode-to-operators-under-orchs-host-ca.md) | [host.md](./host.md), [init.md](./init.md) |
| 4. A/B image upgrade with automatic rollback | built | [ADR 0020](./adr/0020-a-node-boots-one-kernel-file-per-slot-of-the-esp-with-the-initramfs-embedded-and-a-trial-slot-is-reached-once-by-bootnext.md), [ADR 0025](./adr/0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md) | [image.md](./image.md), [init.md](./init.md) |
| 5. System extensions | built | [ADR 0055](./adr/0055-orch-and-the-signer-are-delivered-as-a-system-extension-a-squashfs-image-init-mounts-read-only.md) | [init.md](./init.md) |
| 8. Control-plane HA (several etcd members and a virtual address) | removed with the cluster | [ADR 0114](./adr/0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md) | none |
| 6. Dynamic Node join | removed with the cluster's tokens | [ADR 0114](./adr/0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md) | none |
| 7. Discovery and membership | sketch | none | below |

## The sketch

It is not decided; it is a proposal the PM files and an ADR decides.

7. **Discovery and membership.** Nodes know each other: the set of peers of a cluster, for the etcd peers of a control plane whose members change and for a call forwarded through one Node. The first form is a static list the operator's tool writes, `naructl` as the source; a Node-to-Node protocol waits for a need a list cannot meet. Owner: `naructl` (the list), init (reading it, and forwarding if it comes to that). Depends on: the Node API; forwarding also reopens ADR 0017 item 1. Supersedes: nothing, for the static list. A forwarding init would supersede ADR 0017 item 1.

## Rationale

- [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md): why init serves the API, why a separate authority, and the signs it was wrong.
- [ADR 0018](./adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md): why a base with revisions, the boot choice, and the signs it was wrong.
- [ADR 0002](./adr/0002-gaon-depends-on-naru-one-way-through-the-node-api-and-naru-has-no-control-plane.md): why no concept puts orchestrator logic in init. ADR 0114 supersedes it with the boundary it drew, and this rule stays.

## Open questions

- **How an upgrade bundle is signed.** The call carries the bundle, checked against the release's SHA-256 ([ADR 0025](./adr/0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md)); a signature over it ([image.md](./image.md) §Open questions, Signing) is not decided.
