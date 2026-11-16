# ADR 0070: shared constants are defined once in the leaf module `lib/wire`, one rule says when a disk is blank, and a test parses what the layout writes

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#186, part of goal garamsh/naru#197. It tightens a test of [ADR 0027](./0027-a-node-offers-an-extra-disk-as-xfs-storage-for-mvmds-disks-formatted-when-blank-mounted-hardened-and-grown-at-boot.md) and the blank-disk finding of [ADR 0032](./0032-a-storage-disk-is-found-by-the-xfs-label-init-gives-it-and-device-names-only-a-blank-disk.md), and it reads [ADR 0019](./0019-the-gaon-topology-and-a-nodes-credential-mode-are-defined-once-in-naructls-layout-and-the-harness-reads-the-topology-from-naructl-topology.md) and [ADR 0060](./0060-the-gaon-node-layout-is-the-library-lib-layout-called-by-naructl-and-by-orch-when-it-builds-a-joining-hosts-configuration.md) as they stand.

## Context

- **A value two units share was written out in each.** The data disk's label and mount point, the mkfs path, the ESP's GUID, the host, Node and Kubernetes API ports, etcd's ports and the shape of an enrollment key each had a definition in `lib/layout`, `lib/init`, `naructl`, `orch`, `node` and `tooling/qemu`, kept equal by hand. A change to one that missed another was found at run time, on a Node.
- **Two tests of a blank disk disagreed.** The data disk was blank when its first and last MiB were zero; a storage disk, when its first MiB was zero. A disk that held a backup GPT in its last MiB was formatted as storage and refused as the data disk.
- **The layout writes what init reads, through two YAML libraries, and nothing parsed one with the other.** A field renamed on one side failed on a Node.
- **`lib/init` cannot import `lib/layout` and the reverse test cannot import `lib/init` without a cycle** if the constants sit in the layout: the layout test needs `lib/init`, and `lib/init` would need the layout's constants.

## Decision

1. **The shared constants are the leaf module `lib/wire`**: package `wire`, no dependency but the standard library (testify in its tests). It holds the data disk's label and mount point, the mkfs path, the ESP's GUID, the five ports as text, and the enrollment key's prefix and length with `EnrollKey` and `IsEnrollKey`. A leaf module has no cycle with the layout or with `lib/init`.
2. **Every Go user imports the constant.** `tooling/qemu` imports `lib/wire` and keeps no copy: ADR 0019 forbids it the layout, which writes Nodes, not a module of constants. `node/internal/fence` keeps its two ports as integers, which an nftables rule needs, and a test pins them to `wire`'s text. `image/host/disk/disk.env` is not Go: a test in `lib/wire` reads it and compares its `ESP_GUID` to `wire.ESPGUID`.
3. **One rule says a disk is blank**, `isBlankDisk` in `lib/init/linux`, for the data disk and for a storage disk: the disk holds at least one byte, and its first MiB and its last MiB (all of it when smaller) are all zero. That is no partition table, no file system signature and no backup GPT. The storage test of ADR 0027 and the finding of ADR 0032 used the first MiB alone; they are tightened to this rule, so a storage disk whose last MiB holds anything is left alone, and a zero-byte device is not blank.
4. **`lib/layout` tests against `lib/init/config`.** `lib/layout` requires `lib/init` for its tests only; a test parses the user-data of a worker, a control plane, a control plane of files, a host and the host orch builds, and each must parse and validate.
5. **The YAML libraries stay split.** `lib/layout` writes with `go.yaml.in/yaml/v3`, whose output is byte-stable (ADR 0060 decision 3). `lib/init/config` reads with `go.yaml.in/yaml/v4` and `WithKnownFields`, which it needs to name an unknown field and its line. Moving the writer to v4 changes the bytes every Node's user-data holds; moving the reader to v3 loses the field errors. The parse test of decision 4 is what keeps the two together.

## Consequences

- **A constant is changed once.** `go build` fails at a user that is left behind, and a test fails at `disk.env` and at the fence.
- **`lib/wire` is a module more**, in `go.work`, required with a `replace` by the modules that import it.
- **Some defaults stay written out** where they are flag defaults of a daemon: `host/cmd/naru-hostd`, `orch/cmd/naru-orchd` and `host/cmd/naru-signer`.
- **A storage disk is formatted less often.** A disk with a backup table in its last MiB, which was formatted, is now left alone.

## Signs the decision was wrong

- `lib/wire` grows a constant only one unit reads, which says it is a unit's constant.
- The parse test passes while a Node refuses its own user-data, which says the test's Nodes are not the ones the callers build.
