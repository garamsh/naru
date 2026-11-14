# Target

The shape naru is taking for eight concepts of a Node operating system, what each is in naru, which unit owns it, what it depends on, and the order the unbuilt ones follow.

Claims about this repository's code and documents hold at commit `5b42b429c6b41447f2ea267fe606c2b5d44ea63e`; they promise nothing about later versions. The concepts are those of the Talos Linux Node operating system, cited by concept and not copied. Goal garamsh/naru#44 decided the eight (decision 1) and decided that two are built now, the Node API and the configuration model (decision 2).

## Current decisions

- **Eight concepts are the target.** Two are decided in full and recorded as ADRs: the Node API ([ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)) and the configuration model ([ADR 0018](./adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md)). Six are sketches, in §The six follow-ups, with one enabling step (the reboot endpoint) that several of them need: they bind nothing until an issue and an ADR decide them, and each names the existing decision it would supersede.
- **The Node API is init's.** It is a package of init and not a new unit, which leaves [structure.md](./structure.md) §Units' "node API server, a binary" unclaimed by any decision; whether that line stays is for the PM when ADR 0017 merges.
- **The unit that owns each concept is the unit that owns its state today.** Init owns what runs on a Node (services, files, certificates, the store, the API). `naructl` owns what is written for a Node before it runs and what an operator sends it. `image/` and `tooling/disk` own what is on the boot disk. gaon owns nothing here ([boundaries.md](./boundaries.md): naru does not know gaon's resource API).
- **No concept puts orchestrator logic in init** ([ADR 0002](./adr/0002-gaon-depends-on-naru-one-way-through-the-node-api-and-naru-has-no-control-plane.md) item 6). Where a follow-up needs a decision across Nodes (a VIP's holder, a join), the sketch places it outside init.

## What exists now

At the commit above:

- **Init** ([init.md](./init.md)): PID 1 of a read-only initramfs; boot order of eleven steps; a cgroup per service; restart backoff; the kept console log; SIGTERM powers off and SIGINT reboots. It has no listener, and no way to change the configuration after boot.
- **Configuration** ([config.md](./config.md)): a NoCloud drive's `user-data` (`naru.yaml`) and `network-config`, read once at boot.
- **`naructl`** ([config.md](./config.md) §Operator tooling: naructl): `gen ca`, `gen token`, `gen node`, `gen client`, `gen etcd-client`; the single owner of the gaon Node layout ([ADR 0015](./adr/0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md)).
- **Persistence**: the data disk at `/var/lib/naru`, with `mvmd`, `etcd`, `inputs` and the Node Agent's certificate directory ([ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md), [ADR 0016](./adr/0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md)).
- **The boot disk** ([image.md](./image.md)): one GPT disk with one EFI system partition holding the kernel and `\INITRD.GZ` ([ADR 0007](./adr/0007-the-proxmox-boot-form-is-a-uefi-disk-image-whose-efi-stub-kernel-loads-the-initramfs-from-the-esp.md)). There is one boot slot.
- **etcd** is one member: `naructl/internal/layout` starts it with `--initial-cluster <name>=http://127.0.0.1:2380`, loopback only ([ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md) item 2).
- **Join** is by a bootstrap token per Node, held in a file in `naructl`'s CA directory and in the control-plane Node's user-data ([ADR 0016](./adr/0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md)).
- **Reset and recovery**: a Node that init refuses stays up with no service and no way in but the console ([init.md](./init.md), "A refusal stops every service").

## The eight concepts

Each entry: what it is in naru, the unit that owns it, what it depends on, and the existing decision it supersedes.

### Built in goal #44

1. **Node API** (a machine daemon and an API proxy in one). Init serves HTTPS with JSON and mutual TLS 1.3 from its own authority; `naructl` is the client. Owner: init (server), `naructl` (authority, client). Depends on: nothing new. Supersedes: nothing; it fills in what ADR 0002 left open. [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md).
2. **Configuration model** (a versioned machine configuration, patches, apply modes). The NoCloud user-data is the base; revisions are stored on the data disk; a merge patch with keyed lists (RFC 7386, plus a merge by key for `spec.services`, `spec.files` and the two lists of `spec.pki`) changes a running Node in `no-reboot` or `staged` mode. Owner: init (store, apply, boot choice), `naructl` (building the patch). Depends on: the Node API, the data disk. Supersedes: nothing; it adds a source after [ADR 0008](./adr/0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md)'s. [ADR 0018](./adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md).

### The enabling step

A sketch, not a ninth concept.

- **Reboot endpoint and `reboot` mode.** `POST /v1/reboot`, and an apply mode `reboot` that stores and then reboots. Init already reboots on SIGINT. Owner: init. Depends on: the Node API, the store. Supersedes: nothing. It is the smallest piece here and three of the concepts below need it.

### The six follow-ups

Sketches. None is decided; each is a proposal the PM files and an ADR decides.

3. **Reset, wipe and recovery** (a reset call and a maintenance mode). A call that wipes the store, the data disk or both and reboots; and a mode in which a Node with no valid configuration still serves a limited API, which today it cannot, since init stops before any service starts. Owner: init. Depends on: the reboot endpoint, the store. Supersedes: the part of [init.md](./init.md)'s "A refusal stops every service" that leaves a refused Node with no way in, if a maintenance mode serves the API there. On a control-plane Node, a wipe needs etcd's member removed first, which is the HA follow-up's.
4. **A/B image upgrade with automatic rollback.** Two boot slots on the boot disk, each a kernel and initramfs; a call that writes the inactive slot and selects it for one boot; a rollback to the previous slot if the new one does not report healthy. Owner: `image/` and `tooling/disk` (the layout), init (the call and the health report). Depends on: the reboot endpoint, the store (it survives on the data disk, which a slot change does not touch), and a decision on the installer and the disk layout that [image.md](./image.md) §Open questions leaves open. Supersedes: [ADR 0007](./adr/0007-the-proxmox-boot-form-is-a-uefi-disk-image-whose-efi-stub-kernel-loads-the-initramfs-from-the-esp.md)'s one kernel and one initramfs on one ESP, and [ADR 0004](./adr/0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md) item 2's "no disk layout for the system".
5. **System extensions** (built for the `naru-orch` extension, [ADR 0055](./adr/0055-orch-and-the-signer-are-delivered-as-a-system-extension-a-squashfs-image-init-mounts-read-only.md), which settles the format: a squashfs image on the data disk, named by its SHA-256 in `spec.extensions`). An extension is an image component an operator adds without rebuilding the image: files under a read-only mount and, in the configuration, the services that run them. Owner: `image/` (the format), init (mounting, listing in `GET /v1/services`). Depends on: the A/B slot, if an extension is part of a slot's image, or only the data disk, if it is not; which one is the sketch's open question. Supersedes: [ADR 0004](./adr/0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md)'s consequence that the initramfs holds every binary of P0.
6. **Dynamic Node join** (tokens issued and revoked through the API). The first form needs no new endpoint: a token is a line of `/run/gaon/bootstrap-tokens`, a `spec.files` entry of the control-plane Node with `restart: [gaon-apiserver]`, so `naructl` can add or revoke one with a patch to that Node, which names the `spec.files` entry by `path`. A dedicated token endpoint waits for a reason this form cannot meet. Owner: `naructl` (the token commands, the layout), init (the apply of ADR 0018). Depends on: the Node API and the configuration model, both built here. Supersedes: [config.md](./config.md)'s procedure to add a Node ("apply that new user-data … the control-plane Node restarts with it") and [ADR 0016](./adr/0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md)'s consequence "Adding a Node needs the control-plane Node's user-data again".
7. **Discovery and membership.** Nodes know each other: the set of peers of a cluster, for the HA follow-up's etcd peers and for a call forwarded through one Node. The first form is a static list the operator's tool writes, `naructl` as the source; a Node-to-Node protocol waits for a need a list cannot meet. It cannot read gaon's Node resources (ADR 0002 item 3). Owner: `naructl` (the list), init (reading it, and forwarding if it comes to that). Depends on: the Node API; forwarding also reopens ADR 0017 item 1. Supersedes: nothing, for the static list. A forwarding init would supersede ADR 0017 item 1.
8. **Control-plane HA** (a multi-member etcd and a VIP). More than one Node with `spec.controlPlane`, etcd with several members, and a virtual address that moves to a live control-plane Node. Owner: `naructl` (the layout: etcd's URLs and members, which today are loopback and one), a VIP service the configuration starts (not init, which holds no election, ADR 0002 item 6). Depends on: dynamic join (adding a member), discovery (the peers), reset (removing one), and the Node API. Supersedes: [ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md) item 2's single member on loopback, and the layout's loopback-only etcd URLs.

## Order of the follow-ups

Each line gives what it needs first; a number in this list is a line of this list. Items with no dependency among them can be filed together and run in parallel.

1. **Reboot endpoint and `reboot` mode.** Needs the Node API and the store, so it follows goal #44.
2. **Dynamic join.** Needs the Node API and the configuration model, so it follows goal #44 and does not wait for 1.
3. **Reset, wipe and recovery.** Needs 1.
4. **A/B image upgrade.** Needs 1, and a decision on the installer and disk layout.
5. **System extensions.** Needs 4 if an extension is part of a slot; otherwise nothing but the data disk.
6. **Discovery and membership.** Needs the Node API; it can start after goal #44, and HA needs its static form.
7. **Control-plane HA.** Needs 2, 3 and 6.

## Rationale

- [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md): why init serves the API, why a separate authority, and the signs it was wrong.
- [ADR 0018](./adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md): why a base with revisions, the boot choice, and the signs it was wrong.
- [ADR 0002](./adr/0002-gaon-depends-on-naru-one-way-through-the-node-api-and-naru-has-no-control-plane.md): why no concept puts orchestrator logic in init, and why naru does not read gaon's resources.
- The order follows what each follow-up cannot work without: the reboot call is the one step several others take; join needs only what goal #44 builds; HA is last because it needs three others.

## Open questions

- **Whether [structure.md](./structure.md)'s "node API server, a binary" and [boundaries.md](./boundaries.md)'s open questions on the Node API's shape and PKI stay**, now that ADR 0017 decides both inside init. Those files are edited by another change; they are the PM's to update when ADR 0017 merges.
- **A maintenance mode's trust.** A Node with no valid configuration has no `spec.api` and so no authority to trust; how it accepts an operator is not decided.
- **The upgrade image's path to a Node.** Whether the call carries the image or names a URL, and how an image is signed ([image.md](./image.md) §Open questions, Signing), is not decided.
- **Whether extensions can be added without a kernel option.** Which file-system format an extension takes depends on what the host kernel supports; not checked here.
- **Who holds the VIP.** Which service moves a virtual address, and what tells it a control-plane Node is alive, is not decided.
- **Whether a Node API client library under `lib/` is built.** gaon would require it as an external module ([structure.md](./structure.md) §Units); nothing asks for it yet.
