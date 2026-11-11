# Config

How a Node's configuration and network reach init in P0, and the shape of the files.

Claims about garamsh/gaon hold at commit `d643b1cb6a6cc01ea587dd7c967bdfd07280fe0b`, and about garamsh/mvmd at `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64`; they promise nothing about later versions.

## Current decisions

- **Delivery.** A cloud-init NoCloud drive: a block device with an ISO 9660 volume labelled `cidata` (either case), the form Proxmox's cloud-init drive takes. Init mounts it read-only and reads two files: `user-data`, which is `naru.yaml`, and `network-config`. `meta-data` is not read. It is not `fw_cfg`, and no ext4 `NARUCFG` disk is read.
- **Shape.** `apiVersion: naru.garam.sh/v1alpha1`, `kind: NodeConfig`, `metadata.name` the node name, and `spec`. There is no `status`. The shape follows gaon's resource rule, "every resource has `apiVersion`, `kind`, `metadata`, `spec` and `status`" (gaon `docs/architecture/resource-api.md` §Common rules), minus `status`, which a file has no observer for.
- **`spec` holds:**

  | Field | Meaning |
  |---|---|
  | `services` | a list; each has a name, a binary path, args, env, and `controlPlane: bool`, which marks a service started only on the node that runs the control plane |
  | `controlPlane` | `bool`; whether this node runs the control plane |
  | `dataDisk` | `device` or `label` (exactly one), and `mountPoint` |
  | `network` | `forwarding`: the families, `ipv4` and `ipv6`, for which forwarding is turned on |
| `files` | a list of files init writes before any service starts; each has `path` (absolute, under `/run/`, and not `/run/naru` or below it), `mode` (octal, at most `0777`; `0644` when absent) and `content`. No path is another's directory |

- **The network is `network-config`**, in the version 1 form Proxmox writes (qemu-server `src/PVE/QemuServer/Cloudinit.pm`, at commit `b0965eeba61248046f803a4995112f0ab8523843`). Init accepts only `version: 1`; `physical` cards, each with a `mac_address` or a `name` (the MAC wins when both are given, and init does not rename a link), a static IPv4 subnet (`address` with a dotted `netmask`, or in CIDR form) and at most one `gateway`, which lies inside the prefix; and one `nameserver` entry with `address` and `search` lists. It refuses DHCP, IPv6, bonds, VLANs, bridges, routes, an unknown field and a malformed value. Init sets each link up, adds its address and the default route through netlink, and writes the resolvers to `/run/naru/resolv.conf`, which `/etc/resolv.conf` links to.
- **Init validates before it acts.** An unknown `apiVersion` or `kind`, a field the shape does not name, a value that fails validation, or a file that does not parse, stops init before any service starts; the error names the field. The same holds for a `network-config` that is malformed or asks for what init does not apply, and for a drive that is missing, has no `user-data` or has no `network-config`. What init does next is in [init.md](./init.md).
- **Restart backoff is fixed in P0.** A service that exits is restarted after 1 s, doubling to a cap of 30 s, reset after a run of 60 s. The file has no field for it.
- **Forwarding.** Init turns on `net.ipv4.ip_forward` and `net.ipv6.conf.all.forwarding` for the families `network` names, before it starts mvmd, since mvmd never sets it and refuses to start when a served family has it off (garamsh/mvmd#311).
- **naru knows no service's internals.** Name, binary and arguments come from this file.

## Rationale

- [ADR 0008](./adr/0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md): why a NoCloud drive, why `network-config` is read strictly, why the files field, and the signs it was wrong. It supersedes ADR 0005's delivery.
- [ADR 0005](./adr/0005-configuration-is-a-yaml-file-on-a-labelled-ext4-disk-in-the-shape-of-gaons-resources.md): why gaon's shape and the forwarding duty; its delivery by an ext4 disk is superseded.
- [ADR 0004](./adr/0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md): why P0 has no node API.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): the forwarding obligation, and the other facts mvmd needs from a Node's configuration.

## Open questions

- **The node API.** Its shape, and whether it accepts a `NodeConfig`, is not decided.
- **The node API's PKI.** How a caller is authenticated, and how a first Node accepts one, is not decided.
- **Fields beyond P0.** DHCP, IPv6, several addresses per card and routes in `network-config`, and the host network settings gaon will need for Nodes to connect, are not decided.
- **Configuration integrity.** Whether and how the file is signed is not decided.
