# ADR 0008: A Node reads its configuration and network from a cloud-init NoCloud drive

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#16. It supersedes the delivery of [ADR 0005](./0005-configuration-is-a-yaml-file-on-a-labelled-ext4-disk-in-the-shape-of-gaons-resources.md), which is a labelled ext4 disk; the shape of the file and the forwarding duty in ADR 0005 stand. Claims about Proxmox hold for Proxmox's qemu-server at commit `b0965eeba61248046f803a4995112f0ab8523843`, file `src/PVE/QemuServer/Cloudinit.pm`. Claims about the Linux kernel hold for the longterm 6.18.63 that `image/kernel/pin.env` pins.

## Context

- **Proxmox's one way to hand a guest per-VM data is a cloud-init drive.** `Cloudinit.pm` writes a NoCloud volume: it builds an ISO 9660 image with `genisoimage -iso-level 3 -J -R -V cidata` holding `user-data`, `meta-data` and `network-config`, and writes it to a 4 MiB volume that the guest sees as an `ide-cd` on the AHCI bus. A Node on Proxmox has no other per-VM file channel, so a `NARUCFG` ext4 disk would have to be built by something that is not Proxmox.
- **Proxmox's `network-config` is the version 1 form.** `nocloud_network` writes `version: 1` and a `config:` list. A card is `type: physical` with `name: ethN`, `mac_address` in lower case and `subnets`; a static subnet has `address` and a dotted `netmask`, and an optional `gateway`; other subnet types are `dhcp4`, `dhcp6`, `ipv6_slaac` and `static6`. Resolvers are an entry of `type: nameserver` whose `address` and `search` are lists.
- **The kernel shows long names only through Joliet or Rock Ridge.** Plain ISO 9660 level 1 names are 8.3 upper case, so `user-data` and `network-config` are visible to a kernel without `CONFIG_JOLIET` (Proxmox's `-J -R` writes both extensions).
- **A Node has no address until something gives it one.** The tests passed `ip=` to the kernel, which a Proxmox guest cannot do, and the disk test of garamsh/naru#15 therefore had no network.
- **The workstation has no genisoimage, mkisofs or xorriso, and a test must not need root.**
- **The services need per-Node files.** mvmd reads its JSON configuration from a file; with the data disk carrying it, each Node needed its own data disk image.

## Decision

**Init finds the block device whose ISO 9660 volume label is `cidata`, mounts it read-only, reads `user-data` as `naru.yaml` and `network-config` as the network, applies the network through netlink, and writes the files `naru.yaml` lists under `/run`, all before any service starts.**

1. **The drive is found by label, as the ext4 disks are.** Init reads the volume descriptor of each device (the label is 32 bytes at byte 40 of sector 16) and accepts `cidata` in either case, as cloud-init does. It mounts the volume `ro,nosuid,nodev,noexec`. `meta-data` is not read. The `NARUCFG` disk is no longer read.
2. **`user-data` is `naru.yaml`**, under the validation ADR 0005 set. A new field, `spec.files`, lists files init writes: `path` (absolute, under `/run/`, not under `/run/naru`), `mode` (octal up to `0777`, default `0644`) and `content`. Init writes each before the first service starts, so one data disk image serves every Node and mvmd's JSON configuration is per-Node data on the drive.
3. **`network-config` is read strictly and refuses anything it does not apply.** Init accepts `version: 1`, `physical` cards with static IPv4 subnets (an `address` and `netmask`, or an address in CIDR form), at most one `gateway` in a card's subnet that lies inside its prefix, and at most one `nameserver` entry. A DHCP, IPv6, bond, VLAN, bridge or route entry, an unknown field, or a malformed value stops the boot like an invalid `naru.yaml`. Static IPv4 is what P0 needs; the other forms wait for a use.
4. **Init applies the network through netlink**: it finds each link by MAC address, or by name when the card names none, waiting up to 10 s for it, then sets it up, adds the address (`RTM_NEWADDR`, replacing one already there) and the default route (`RTM_NEWROUTE`, likewise). It also sets `lo` up first, which the kernel did only for `ip=`, and a service that listens on 127.0.0.1 needs it. It does not rename a link.
5. **Resolvers are written to `/run/naru/resolv.conf`**, and the initramfs holds `/etc/resolv.conf` as a symbolic link to it, because the root is read-only when a service starts. The directory `/run/naru` is reserved to init.
6. **Netlink is `github.com/vishvananda/netlink`** v1.3.1 (with `netns` v0.0.5 and `golang.org/x/sys` v0.49.0 beneath it, the version the workspace resolves). The standard library has no route-netlink API, and the `syscall` package is frozen. The GitHub advisory database listed no advisory for `netlink` or `netns`, and for `golang.org/x/sys` only GHSA-p782-xgp4-8hr8, which affects versions before `0.0.0-20220412211240-33da011f77ad`.
7. **The kernel gains** `CONFIG_ISO9660_FS`, `CONFIG_JOLIET`, and the drivers a Proxmox guest's drive and disks need: `CONFIG_ATA`, `CONFIG_SATA_AHCI`, `CONFIG_SCSI`, `CONFIG_BLK_DEV_SD`, `CONFIG_BLK_DEV_SR` and `CONFIG_SCSI_VIRTIO`, all built in; `image/kernel/required.txt` pins them.
8. **The QEMU tests lay a Node out as Proxmox does**: q35 under OVMF, the boot and data disks as `scsi-hd` on a `virtio-scsi-pci` controller, the cidata drive as an `ide-cd` on the AHCI bus, and a virtio NIC with a locally administered MAC. Every Node boots `naru.img`, with no `-kernel`, `-initrd`, `-append` or `ip=`. `tooling/qemu/internal/iso9660` writes the volume, with a primary and a Joliet tree, without a tool or root. The address plan stays `10.78.0.<i>/24`, now in each Node's `network-config`.

## Consequences

- A Node on Proxmox is configured by the cloud-init drive the host already makes, and on QEMU by a drive a test writes in Go.
- The network is applied by init before any service, so a service sees its address and default route from its first instruction.
- A malformed `network-config` refuses the boot, so a Node never comes up half-configured on a network it was not given.
- `spec.files` puts a service's configuration in `naru.yaml`, which is not confidential: whoever holds the drive can read it, mvmd's included.
- Init gains a third-party dependency, one module with two beneath it, which the PM's pin review covers.
- The kernel is larger by the SCSI, AHCI and ISO 9660 code.
- naru maintains a small ISO 9660 writer for tests. It was checked against `7z`, which listed and extracted the names and contents.

## Signs the decision was wrong

- Proxmox changes the NoCloud volume (the label, the `network-config` version, the `nameserver` entry) and a real guest refuses to boot.
- Operators need DHCP, IPv6 or several addresses per card, so the strict parser refuses what a host legitimately writes.
- The test writer's volume reads in the kernel and not in a Proxmox-made one, which would show the test no longer stands for Proxmox.
- The netlink module is found to carry an advisory that its pinned version has, or goes unmaintained.

## Errata

- 2026-11-15: paths moved in the image split ([ADR 0054](./0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)): `image/kernel/pin.env` is `image/common/kernel/pin.env` and `image/kernel/required.txt` is `image/host/kernel/required.txt`.
