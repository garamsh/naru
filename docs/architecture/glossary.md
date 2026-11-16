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
| Machine | mvmd's and gaon's word for a microVM. These documents write microVM and keep Machine where they quote them, and in the Machine prefix. | the Machine prefix: the /24 orch gives a host for its microVMs (`spec.host.network`, [orch.md](./orch.md)) |
| member | One of three things, and a document says which. A control host's member of orch (`naru-orchd` with its etcd). A member of a group: the record of one microVM. A member of a network: a microVM's address and Pod prefix. | `naructl gen node --orch-member`, `spec.orch` ([orch.md](./orch.md)); `GET /v1/groups` ([groups.md](./groups.md)); `PUT /v1/networks/{name}` ([host.md](./host.md)) |
| control plane Node | The Node that runs gaon's control plane services and etcd. It is not a control host. | `spec.controlPlane`; `naructl gen node --control-plane` |

The word Node in the older documents named the physical machine, and `boundaries.md` said so. It does not: a Node can be a microVM ([ADR 0034](./adr/0034-a-node-boots-as-a-microvm-from-a-guest-kernel-and-initramfs-the-image-carries-as-a-boot-set.md)), and a physical machine is a host.

## Groups of machines

| Term | Meaning | Maps to |
|---|---|---|
| fleet | The hosts one orch has admitted, which it builds from one host profile and reaches through the host API. | `naructl host list`; `naructl host profile`; `GET /v1/profile`; the console's `Fleet` ([console.md](./console.md)) |
| control host | A host that runs a member of orch and, with it, the signer. | `spec.orch`, `spec.signer`; the `naru-orch` system extension ([ADR 0055](./adr/0055-orch-and-the-signer-are-delivered-as-a-system-extension-a-squashfs-image-init-mounts-read-only.md)) |

## How a host comes to be in a fleet

| Term | Meaning | Maps to |
|---|---|---|
| join form | The configuration of a host that has nothing yet: orch's URL, the SHA-256 of orch's CA and an enrollment key. Init runs `naru-hostd --join` for it, and applies the configuration orch builds at admission ([ADR 0058](./adr/0058-a-host-boots-from-a-join-form-that-names-an-orch-and-an-enrollment-key-and-init-applies-the-configuration-hostd-brings-back.md)). | `spec.join` ([config.md](./config.md)) |
| install seed | The NoCloud `cidata` ISO 9660 volume that carries a join form and nothing else, which a new host boots the installer with ([ADR 0059](./adr/0059-orch-admits-a-host-that-boots-from-an-install-seed-allocates-its-machine-prefix-and-builds-its-configuration-from-one-fleet-profile.md)). The issue that asked for this file calls it the installation seed. | `naructl host install-seed`; `/install-seed` of the console |
| enrollment key | The secret in a join form that lets a host ask to enroll. Orch keeps only its hash. | `naructl enroll-key create`, `naructl enroll-key revoke` |
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
| operator | A person who runs a fleet. The signer's role `operator` is the certificate that person holds; it signs admission statements and can revoke any serial that is not root's. | `naructl operator add`, `naructl operator revoke`; [signer.md](./signer.md) |
| admin | The Node API role `naru:admin`: a client certificate from the Node API authority whose Organization is `naru:admin`. It reads and changes a Node. It is a role on one Node and not a person. | `naructl gen api-client`; [init.md](./init.md) |
| root person | The person who holds the root certificate and its encrypted person file. Root alone issues operator, reader, automation and orch certificates. A new root comes from `naru-signer init` or from the recovery key's reissue. | `naructl operator request`, `naructl operator person`; `naructl signer reissue-root` |
| the four authorities | What a signer answers to `GET /v1/ca`: the host authority, the operator authority and the orch authority, which sign certificates, and the revocation key, which signs the revocation list. All four are in the signer's private directory. | `lib/identity` `Authorities`; `naructl signer ca`; [signer.md](./signer.md) |
| other authorities | Files `naructl` writes with `--ca-dir` and that no signer holds: the cluster authority of gaon's certificates (`gen ca`), the Node API authority (`gen api-ca`), the host API authority (`gen host-ca`) and the Kubernetes front-proxy authority (`gen kubernetes`). | [config.md](./config.md) |

## Code layout

| Term | Meaning | Maps to |
|---|---|---|
| unit | A top-level folder of one responsibility, of one or more Go modules, that imports no other unit. | `host/`, `node/`, `orch/`, `naructl/`, `tooling/` ([structure.md](./structure.md)) |
| module | A Go module: a folder with a `go.mod`, listed in `go.work`. A unit has one or more; `lib/` is not a unit and holds one module per library. | `go.work`; `make lint-imports` |
