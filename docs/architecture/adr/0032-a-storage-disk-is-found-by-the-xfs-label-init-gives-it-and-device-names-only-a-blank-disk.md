# ADR 0032: a storage disk is found by the XFS label init gives it, and `device` names only a blank disk

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#96. It amends [ADR 0027](./0027-a-node-offers-an-extra-disk-as-xfs-storage-for-mvmds-disks-formatted-when-blank-mounted-hardened-and-grown-at-boot.md) by reference: its body is not edited, and what it says holds with this decision added, except item 2's finding of a blank disk by its `device` and the third bullet of item 4, which the items below replace.

## Context

- **A kernel name is not stable across boots.** The disks of one SCSI controller are scanned asynchronously, so `sdb` and `sdc` can trade places. ADR 0027 item 2 found a blank disk by the `device` path it was given and, at every later boot, by the same path.
- **Seen in the QEMU storage tests on 2026-11-14.** Of three runs of the storage tests, one booted with the data disk as `/dev/sdc`; init refused the entry because `/dev/sdc` held ext4, and mvmd then wrote its disks into the data disk, where every later boot refuses the mount because the directory holds files.
- **The safety rule held.** Init formatted no disk that held data. What failed is that the storage disk was missed.
- **init already labels what it formats.** `mkfs.xfs -L <name>` gives the disk the entry's `name`, which is at most 12 characters because it is an XFS label.

## Decision

1. **An entry given by `device` is found first by its `name`.** At each boot init scans the block devices for an XFS file system labelled the entry's `name` and uses that disk wherever the kernel put it. A GPT partition name is not matched in this scan.
2. **`device` is used only when no such XFS exists, and then only for a blank disk,** which is formatted with that label as before. A device that holds anything other than a blank disk is left alone.
3. **A `device` that holds an XFS file system labelled otherwise is left alone and logged** (`storage disk not prepared`, naming both labels), as a disk that holds another file system is. This replaces ADR 0027 item 4's "init does not format it, whatever its label", which mounted such a disk.
4. **The data disk is never a storage disk, whatever path an entry names.** It holds ext4, which is neither blank nor XFS, so no `device` path that now points at it is formatted or mounted.
5. **An entry given by `label` is found as before:** a GPT partition name or an XFS label.

## Consequences

- A Node whose disks change names between boots finds its storage disk by its label, and `marker` files on it are kept.
- A first boot still depends on the name. When no XFS carries the label, `device` must name the blank disk; where the kernel names it otherwise at that boot, `device` is the data disk, init leaves it alone, the entry stays unprepared and the boot goes on. An operator who cannot predict the name gives the disk a GPT partition name or an XFS label and uses `label`.
- A disk formatted before this decision carries the entry's `name` as its label, since init always formatted with it, so it is found at the first boot with this decision. A disk an operator labelled by hand with another name and gave by `device` is now left alone, and the operator renames the entry or the label.
- The QEMU storage tests attach the data and the extra disk as virtio-blk disks, which the kernel names `/dev/vda` and `/dev/vdb` in the order attached, so that the first boot's names are known and a test can swap them.

## Signs the decision was wrong

- A Node is found with a storage disk that was not formatted at its first boot because `device` named the data disk: `device` is too weak a first-boot handle, and the first boot needs another (a size, a serial number, a position).
- A disk labelled `name` by hand holds something that is not the operator's storage disk and is mounted: the label is too weak a mark, and a second one (a UUID kept in the entry) replaces it.
