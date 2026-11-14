# ADR 0035: the host API creates a microVM of an OS form from a boot set, with a data disk and a NoCloud seed, and the ISO 9660 writer is a library module

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#98, part B (decisions 1 and 2 of the PM's comment on that issue). It extends [ADR 0028](./0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md) by reference: its body is not edited, and what it says holds with this decision added. The image the microVM boots is part A's (ADR 0034, which that part adds); this decision is the host API's side.

## Context

- **mvmd has an OS form.** `CreateMachine` takes `os_boot` (a boot set the node offers, and a kernel command line to add) in place of an image, and then makes no root disk and runs no Guest Agent. `routed_prefixes` gives the Machine prefixes mvmd routes to it. `CreateDisk` takes `contents` (at most 1 MiB, with no file system), and a disk attaches only to a Machine that is Created or Stopped, taking effect at the next boot.
- **A Node booted as a microVM needs its seed from the host.** Its configuration is the NoCloud volume of [ADR 0008](./0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md), and its address is the one mvmd gives the Machine, which is known only after the Machine exists.
- **mvmd does not report the gateway.** The Machine's addresses are plain IPs, and the gateway is mvmd's `network.ipv4_gateway`, which is configuration.
- **Two units write the same volume.** `tooling/qemu` wrote the ISO 9660 volume of a test Node's drive in an internal package, and hostd now needs the same writer.

## Decision

1. **A request is of the image form or the OS form.** `POST /v1/microvms` with any of `bootSet`, `cmdline`, `seed`, `dataDisk` or `podPrefix` is the OS form. It takes `name`, `cpu`, `memory`, `bootSet` (a boot set mvmd offers), `cmdline` (optional, at most 2048 bytes, no NUL or newline), `seed` (the user-data, base64), `dataDisk` (bytes), `podPrefix` (optional, an IPv4 prefix of /16 to /32 with no host bits) and `networkPolicy`. It carries none of `image`, `rootDiskBytes`, `disks`, `env` or `command`; a request that mixes the forms is refused with 422 naming the image form's fields it carries. The image form is unchanged.
2. **hostd creates, in this order: the data disk `<name>.data`, the Machine with `os_boot`, the data disk and `routed_prefixes: [podPrefix]`, then the seed disk `<name>.seed`, attached read-only, and then starts the Machine.** The seed is made after the Machine because its `network-config` holds the address mvmd gave it.
3. **The seed is a NoCloud volume labelled `cidata`** with `user-data` (the request's `seed`), `meta-data` (`instance-id` and `local-hostname`, the name) and `network-config`: version 1, one physical card `eth0` with the Machine's address, its netmask and the gateway. The netmask is the length of the prefix mvmd reports in runtime info as the one that holds the address. The volume is created by `CreateDisk` with `contents` and no file system. A volume over 1 MiB is refused with 422 on `seed`, before anything is created; the body limit is 4 MiB so the refusal is reachable.
4. **The gateway is a flag of hostd.** `--machine-gateway` is an IPv4 address; naructl writes the first address of the Node's Machine prefix, which is also what it writes as mvmd's `network.ipv4_gateway`. A host with none refuses an OS-form request with 501 and the image form is unaffected. No nameserver is written, since init's `network-config` takes one entry and hostd does not know the Machines' resolver.
5. **A failure removes what this request made, and nothing else.** A refused `CreateDisk` (a disk of that name exists) is another request's and is left. After the Machine exists, a failure at the seed's creation or attachment deletes the Machine, then the disks this request made. A failed start leaves the Machine, as the image form does, and the same request again starts it.
6. **Idempotency by name holds.** An existing Machine is compared by boot set, command line, routed prefixes, resources, policy, the data disk and its size, and the seed attached read-only with a volume; the seed's contents are not compared. A different body is 409.
7. **The ISO 9660 writer is the module `lib/iso9660`,** `github.com/garamsh/naru/lib/iso9660`, required by `host` and `tooling/qemu`; `tooling/qemu/internal/iso9660` is removed. Each module that requires it carries a `replace` to the folder, so `GOWORK=off` builds resolve it.
8. **hostd's mvmd pin moves to d19cecb1726835871342af6d91a092c448f1f1a7,** which has the OS form. The image's pin (`image/initramfs/pin.env`) is part A's.

## Consequences

- A control plane above the Node starts a Node of the OS form with one request, and a delete removes the Machine and both disks, since both are named `<name>.*`.
- The seed of a microVM that mvmd made but whose seed step was interrupted by a crash of hostd stays Created with no seed. A retry answers 409 (the Machine does not match), and the operator deletes it.
- The microVM's address is not chosen by the caller: it is what mvmd gave, and a caller reads it from the microVM.
- naructl's flags for the OS form are not part of this decision.

## Signs the decision was wrong

- mvmd reports the gateway in its API: the flag goes and hostd reads it from the Machine.
- A seed needs more than 1 MiB, or a nameserver: the limit moves with mvmd's, and the request gets a field for the resolver.
- A crash between the Machine and the seed is met in practice: the retry completes the seed in place of answering 409.

## Errata

- 2026-11-15: decision 2 names the data disk `<name>.data` without saying what is on it. hostd created it with `CreateDisk`, which makes mvmd put an ext4 file system on it, so init found no blank disk to format and the microVM did not boot. The data disk is created raw (`CreateRawDisk`, no file system), and init formats it because the Node's user-data carries `spec.dataDisk.formatBlank` (`naructl gen kubernetes-node` writes it). The decision stands.
