# ADR 0079: a blank storage disk is named by its serial number, which init reads from sysfs, and the XFS label takes over after the first boot

> Status: accepted
> Date: 2026-11-16

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#236, part of garamsh/naru#235. It amends [ADR 0032](./0032-a-storage-disk-is-found-by-the-xfs-label-init-gives-it-and-device-names-only-a-blank-disk.md) by reference: its body is not edited, and what it says holds with this decision added. It adds a third way to name a disk to [ADR 0027](./0027-a-node-offers-an-extra-disk-as-xfs-storage-for-mvmds-disks-formatted-when-blank-mounted-hardened-and-grown-at-boot.md) item 1.

## Context

- **The first boot still depends on a kernel name.** ADR 0032 finds a disk that init has formatted by its XFS label wherever the kernel put it, but a blank disk has no label, so `device` names it, and its Consequences record that the name may be the data disk's at that boot, in which case the entry stays unprepared. aurora met this on Proxmox, where `/dev/sdX` varies between boots (garamsh/naru#235).
- **A stable path does not exist on a Node.** The image has no udev, so `/dev/disk/by-id` is absent (ADR 0027 item 2). The kernel does publish a disk's serial number in sysfs, and Proxmox sets one with `serial=` on a disk.
- **A size is not a handle.** Two disks of one size are common, and a rule that picks by size picks the wrong one without a sign.

## Decision

1. **`spec.storage[]` takes a third field, `serial`,** the serial number of a blank disk. Exactly one of `label`, `device` and `serial` is given; a configuration that gives none, or more than one, is refused, naming `spec.storage[i]`.
2. **`naructl gen node --storage` and `gen host-profile --storage` take `<name>=serial:<NUMBER>`.** A value that starts with `serial:` is the serial, one that starts with `/dev/` is the device and any other is the label, as before. `serial:` with nothing after it is refused.
3. **Init reads a disk's serial from sysfs, in `/sys/class/block/<dev>/`:** the file `serial` (a virtio-blk disk), the payload of `device/vpd_pg80` after its four-byte header (a SCSI disk's VPD page 0x80) or the file `device/wwid`. The disk matches when one of them, trimmed of spaces, line ends and NULs, equals the serial exactly.
4. **An entry given by `serial` is found in this order,** at every boot:
   1. an XFS file system labelled the entry's `name`, wherever the kernel put it (ADR 0032 item 1);
   2. otherwise the one disk of that serial, which is formatted with that label when it is blank, as for `device` (ADR 0032 item 2).

   After the first boot the label finds the disk, so the serial is read only when no labelled XFS exists.
5. **A serial that matches no disk is waited for, as a missing disk is,** up to 10 s, and then the entry is logged as not prepared and the boot goes on. **A serial that matches more than one disk finds none:** init logs the entry as not prepared, naming the devices, and formats nothing, since it cannot tell which was meant. The serial of a disk that holds anything other than a blank disk is left alone, as for `device`; an XFS at it labelled otherwise than the entry's `name` is left alone and logged, as ADR 0032 item 3 says for `device`.
6. **The data disk is never taken,** as ADR 0032 item 4 says: it holds ext4, which is neither blank nor XFS.
7. **The forms are for different disks.** `label` names a disk that already carries a partition name or an XFS label (found before and after, never formatted); `serial` names a blank disk by a handle that survives a reorder, and is formatted once; `device` remains for a Node whose disks have no serial and whose first-boot names are known.

## Consequences

- A host whose disks are named differently at each boot finds and formats the intended blank disk at its first boot and the same disk by its label afterwards, with no device path in its configuration, provided the platform gives the disk a serial (Proxmox `serial=`).
- A disk with no serial, or two with the same, cannot be named this way; the first is named by `device` or `label` and the second is left unprepared and reported.
- A disk whose serial an operator later changes keeps its label, so it is still found; the serial matters only until the first format.
- The QEMU storage tests attach the extra disk with `serial=`, which both virtio-blk and SCSI disks of QEMU carry.

## Signs the decision was wrong

- Operators regularly have disks without a serial, or with duplicated ones: a handle such as a position on the bus replaces it, or the first-boot format asks for an explicit operator choice.
- A serial read from sysfs differs between kernels for one disk (a whitespace or a prefix): the match becomes a prefix or a normalised form.
