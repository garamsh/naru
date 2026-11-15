# ADR 0072: an admitted host boots from its stored configuration and network with no seed attached

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#216, part of goal garamsh/naru#197. It amends [ADR 0008](./0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md) and [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) by reference, and it reads [ADR 0058](./0058-a-host-boots-from-a-join-form-that-names-an-orch-and-an-enrollment-key-and-init-applies-the-configuration-hostd-brings-back.md) and [ADR 0070](./0070-shared-constants-are-defined-once-in-the-leaf-module-lib-wire-one-rule-says-when-a-disk-is-blank-and-a-test-parses-what-the-layout-writes.md) as they stand.

## Context

- **Init refused a boot with no NoCloud drive.** It read the drive first, on every boot, and a missing one ended the boot with `NoCloud drive "cidata": no disk with that label`. The stored configuration was chosen only when its base digest matched the seed's user-data.
- **The seed of a host that joined carries the enrollment key.** The operator is told to revoke the key and destroy the seed once the host is admitted, but the host could not boot without the seed, so the seed stayed attached for the host's life. aurora met this on v0.1.0-dev.11 (aurora#808): it detached the seed after `join configuration applied` and the host stayed down.
- **The network came only from the seed.** A boot with no seed has the configuration on the data disk and no network-config.

## Decision

1. **With no NoCloud drive attached, init runs the stored configuration.** `Host.ReadSeed` returns an error wrapping `boot.ErrNoSeed` when no disk carries the label, after the wait for a disk. Init then finds the data disk by `wire.DataLabel` and mounts it at `wire.DataMount`, and never formats it on this path. It runs the stored configuration when one exists and validates. There is no base, so its digest is not checked.
2. **The network is stored beside the configuration.** Every boot that has a seed writes the seed's network-config (empty when the seed has none) to `network-config` in the store's directory, through `atomicfile.Write`. A boot with no seed brings the network up from that file. It mounts the data disk first, since the file is on it.
3. **A boot with no seed refuses in three cases, each naming both reasons**: the data disk cannot be mounted; there is no stored configuration, or it does not validate; there is no stored network-config. The last is a host that last booted before init stored the file, so the refusal reads `no stored network-config; attach the seed for one boot`. A network is not guessed: DHCPv4 on a host configured for a static address brings it up on a network nobody configured.
4. **A seed attached again wins**, under the digest rule of ADR 0018: a stored configuration of another base is discarded, and the seed's network-config is stored again. A Node whose data disk has a label other than `NARUDATA` needs its seed.
5. **The enrollment key may be revoked while the seed is attached.** A dev.11 host keeps booting from its seed after the key is revoked. From dev.12 the seed may also be detached, once the host has booted once on dev.12 with it attached.

## Consequences

- **The key stops being a lifelong secret on the host.** The seed, and the key in it, can be destroyed after admission.
- **A boot with no seed waits for a disk.** The wait for the NoCloud drive (up to 10 s, ADR 0008) runs before the seedless path, so a seedless boot starts that much later.
- **A host that is moved to dev.12 and loses its seed before booting it is down** until the seed is attached once.
- **`lib/init/store` keeps a second file**, `network-config`, and the `Configs` interface of `lib/init/boot` two methods more.

## Signs the decision was wrong

- A host boots seedless on a stale `network-config` after its network changed, which says the file needs a revision of its own and not a copy of the seed's.
- An operator detaches a seed to remove the key and the host refuses for a reason the console does not make plain.
