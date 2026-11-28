# Glossary

The words the architecture documents use, what each names, and the flag, field or path it maps to. A document uses a term as this file defines it; where a document needs a second meaning, it says so in its own words.

## Contents
- Machines and what runs on them
- Groups of machines
- How a host comes to be in a fleet
- Boot sets and disks
- People and authorities
- Code layout

## Machines and what runs on them

| Term | Meaning | Maps to |
|---|---|---|
| Node | A system naru's init boots from a `NodeConfig`: the operating system naru owns ([boundaries.md](./boundaries.md)). It is booted from the host image or the node image; the word does not say which. | `kind: NodeConfig`, `metadata.name` ([config.md](./config.md)); `naructl --endpoint` |
| host | A Node booted from the host image: a machine that runs mvmd and `naru-hostd` and so serves microVMs. Often hardware, and it can itself be a virtual machine. | the `host/` unit; `spec.host`; `naru-hostd`, [host.md](./host.md) |
| microVM | A virtual machine that mvmd runs on a host. A microVM of the OS form boots the node image from a boot set and is a Node. | `POST /v1/microvms`; `naructl microvm` |
| Machine | Two meanings, and a document says which. mvmd's word for a microVM on one host, which these documents write microVM and keep as Machine where they quote mvmd, and in the Machine prefix. And naru's resource of the Machine API: a Machine across hosts that a caller names and orch places, moves and routes; on its host it is one microVM at a time ([machines.md](./machines.md)). | the Machine prefix: the /24 orch gives a host for its microVMs (`spec.host.network`, [orch.md](./orch.md)); `PUT /machines/v1alpha1/machines/{name}` ([ADR 0108](./adr/0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md), decided, not built) |
| use | A service above naru that calls the Machine API only and maps its own names to Machines: garam's Workspace, a later Function service. | none in naru: naru knows no use |
| shape | What a use asks of a Machine's host instead of naming one: region, labels and GPUs. | `shape` of a Machine ([machines.md](./machines.md) §Placement) |
| capability | What a Machine can do on its host, reported per Machine: the snapshot and move kinds, resize, GPUs. | `status.capabilities` ([machines.md](./machines.md) §The API) |
| stable name | A Machine's name in the zone, `m-<id>.<zone>` for its default service and `m-<service>--<id>.<zone>` for another, which no move changes. | the Machine API's `status.address`; `--machine-zone` ([proxy.md](./proxy.md)) |
| alias | A hostname a use publishes for one of its Machines, in a domain the operator granted the caller and never in the zone. | `PUT /machines/v1alpha1/aliases/{hostname}` ([proxy.md](./proxy.md)) |
| mesh address | A host's own /32 on its WireGuard tunnel, from the profile's `meshPool`, from which its traffic to other hosts goes. | `GET /v1/tunnel`; `meshPool` ([ADR 0113](./adr/0113-hosts-form-a-wireguard-mesh-with-an-address-each-hosts-that-cannot-be-reached-dial-out-to-relays-and-orch-rotates-every-hosts-key-amending-adr-0045-item-5-adr-0048-item-5-and-adr-0049-item-6.md)) |
| relay | A reachable host that carries the mesh for hosts nobody can dial. | `naru.garam.sh/relay` ([ADR 0113](./adr/0113-hosts-form-a-wireguard-mesh-with-an-address-each-hosts-that-cannot-be-reached-dial-out-to-relays-and-orch-rotates-every-hosts-key-amending-adr-0045-item-5-adr-0048-item-5-and-adr-0049-item-6.md)) |
| volume | A disk of a Machine that lives as long as the Machine and moves with it; the root disk does not. | `volumes` of a Machine; an mvmd disk on its host |
| binding | Orch's record of the host a Machine is placed on and the epoch of that placement; the microVM is `m-<id>-<epoch>`. | `/naru/orch/machines/` ([machines.md](./machines.md) §State) |
| member | One of three things, and a document says which. A control host's member of orch (`naru-orchd` with its etcd). A member of a group: the record of one microVM. A member of a network: a microVM's address. | `naructl gen node --orch-member`, `spec.orch` ([orch.md](./orch.md)); `GET /v1/groups` ([groups.md](./groups.md)); `PUT /v1/networks/{name}` ([host.md](./host.md)) |

The word Node in the older documents named the physical machine, and `boundaries.md` said so. It does not: a Node can be a microVM ([ADR 0034](./adr/0034-a-node-boots-as-a-microvm-from-a-guest-kernel-and-initramfs-the-image-carries-as-a-boot-set.md)), and a physical machine is a host.

## Groups of machines

| Term | Meaning | Maps to |
|---|---|---|
| fleet | The hosts one orch has admitted, which it builds from one host profile and reaches through the host API. | `naructl host list`; `naructl host profile`; `GET /v1/profile`; the console's `Fleet` ([console.md](./console.md)) |
| control host | A host that runs a member of orch and, with it, the signer. | `spec.orch`, `spec.signer`; the `naru-orch` system extension ([ADR 0055](./adr/0055-orch-and-the-signer-are-delivered-as-a-system-extension-a-squashfs-image-init-mounts-read-only.md)) |

## How a host comes to be in a fleet

| Term | Meaning | Maps to |
|---|---|---|
| join form | The configuration of a host that has nothing yet: orch's URL, the SHA-256 of the key of orch's authority and an enrollment key. Init runs `naru-hostd --join` for it, and applies the configuration orch builds at admission ([ADR 0058](./adr/0058-a-host-boots-from-a-join-form-that-names-an-orch-and-an-enrollment-key-and-init-applies-the-configuration-hostd-brings-back.md)). | `spec.join` ([config.md](./config.md)) |
| install seed | The NoCloud `cidata` ISO 9660 volume that carries a join form and nothing else, which a new host boots the installer with ([ADR 0059](./adr/0059-orch-admits-a-host-that-boots-from-an-install-seed-allocates-its-machine-prefix-and-builds-its-configuration-from-one-fleet-profile.md)). The issue that asked for this file calls it the installation seed. | `naructl host install-seed`; `/install-seed` of the console |
| enrollment key | The secret in a join form that lets one host ask to enroll, within 24 hours; the host's admission spends it. Orch keeps only its hash. | `naructl enroll-key create`, `naructl enroll-key revoke` |
| admission | An operator's approval of a pending host, after which orch builds the host's configuration and has the signer issue its certificate. | `naructl host admit`; `POST /v1/admit` |

## Boot sets and disks

| Term | Meaning | Maps to |
|---|---|---|
| boot set | A kernel and an initramfs that mvmd offers as a unit, by name, to a microVM of the OS form. The release carries the node image's as a kernel, an initramfs and a manifest. | `bootSet` of `POST /v1/microvms`; `POST /v1/boot-sets`; `naructl host boot-set add`, `naructl host boot-set remove` ([ADR 0046](./adr/0046-the-host-api-adds-and-removes-a-boot-set-on-a-running-host-through-mvmds-runtime-api-and-a-release-carries-the-node-boot-set-as-a-kernel-an-initramfs-and-a-manifest.md)) |
| blank disk | A disk of at least one byte whose first and last MiB are all zero: no partition table, no file system signature, no backup GPT. One rule, `isBlankDisk` in `lib/init/linux`, decides it for the data disk and for a storage disk ([ADR 0070](./adr/0070-shared-constants-are-defined-once-in-the-leaf-module-lib-wire-one-rule-says-when-a-disk-is-blank-and-a-test-parses-what-the-layout-writes.md)). | `spec.dataDisk.formatBlank`; `spec.storage[].device` |
| data disk | The disk labelled `NARUDATA`, mounted at `/var/lib/naru`, which holds the stored configuration, etcd, mvmd's state and the other persistent directories. | `spec.dataDisk`; `lib/wire` |

## People and authorities

| Term | Meaning | Maps to |
|---|---|---|
| caller | A use's provider at the place it runs, holding a certificate of the role `caller` named by its CommonName, which reaches only the Machine API and its own Machines. It registers once with a one-time token and renews itself ([ADR 0108](./adr/0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md), [ADR 0116](./adr/0116-a-caller-registers-once-with-a-one-time-token-root-asks-the-signer-for-and-a-key-it-makes-where-it-runs-and-renews-its-own-certificate-superseding-adr-0108-item-4s-issuance.md); the signer's calls are built, the Machine API is not). | `naructl caller token`, `naructl caller revoke`; the signer's `POST /v1/caller-register`, `POST /v1/caller-renew` and `POST /v1/revoke-caller` |
| operator | A person who runs a fleet. The signer's role `operator` is the certificate that person holds; it signs admission statements and can revoke any serial that is not root's. | `naructl operator add`, `naructl operator revoke`; [signer.md](./signer.md) |
| admin | The Node API role `naru:admin`: a client certificate from the Node API authority whose Organization is `naru:admin`. It reads and changes a Node. It is a role on one Node and not a person. | `naructl gen api-client`; [init.md](./init.md) |
| root person | The person who holds the root certificate and its encrypted person file. Root alone issues operator, reader, automation and orch certificates. A new root comes from `naru-signer init` or from the recovery key's reissue. | `naructl operator request`, `naructl operator person`; `naructl signer reissue-root` |
| the four authorities | What a signer answers to `GET /v1/ca`: the host authority, the operator authority and the orch authority, which sign certificates, and the revocation key, which signs the revocation list. All four are in the signer's private directory. | `lib/identity` `Authorities`; `naructl signer ca`; [signer.md](./signer.md) |
| other authorities | Files `naructl` writes with `--ca-dir` and that no signer holds: the Node API authority (`gen api-ca`) and the host API authority (`gen host-ca`). | [config.md](./config.md) |

## Code layout

| Term | Meaning | Maps to |
|---|---|---|
| unit | A top-level folder of one responsibility, of one or more Go modules, that imports no other unit. | `host/`, `node/`, `orch/`, `naructl/`, `tooling/` ([structure.md](./structure.md)) |
| module | A Go module: a folder with a `go.mod`, listed in `go.work`. A unit has one or more; `lib/` is not a unit and holds one module per library. | `go.work`; `make lint-imports` |
