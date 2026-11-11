# Config

How a Node's configuration reaches init in P0, and the shape of the file.

Claims about garamsh/gaon hold at commit `d643b1cb6a6cc01ea587dd7c967bdfd07280fe0b`, and about garamsh/mvmd at `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64`; they promise nothing about later versions.

## Current decisions

- **Delivery.** A small ext4 disk with file system label `NARUCFG` holds one file, `naru.yaml`. Init mounts it read-only. It is not `fw_cfg`.
- **Shape.** `apiVersion: naru.garam.sh/v1alpha1`, `kind: NodeConfig`, `metadata.name` the node name, and `spec`. There is no `status`. The shape follows gaon's resource rule, "every resource has `apiVersion`, `kind`, `metadata`, `spec` and `status`" (gaon `docs/architecture/resource-api.md` §Common rules), minus `status`, which a file has no observer for.
- **`spec` holds:**

  | Field | Meaning |
  |---|---|
  | `services` | a list; each has a name, a binary path, args, env, and `controlPlane: bool`, which marks a service started only on the node that runs the control plane |
  | `controlPlane` | `bool`; whether this node runs the control plane |
  | `dataDisk` | `device` or `label` (exactly one), and `mountPoint` |
  | `network` | `forwarding`: the families, `ipv4` and `ipv6`, for which forwarding is turned on |

- **Init validates before it acts.** An unknown `apiVersion` or `kind`, a field the shape does not name, a value that fails validation, or a file that does not parse, stops init before any service starts; the error names the field. What init does next is in [init.md](./init.md).
- **Restart backoff is fixed in P0.** A service that exits is restarted after 1 s, doubling to a cap of 30 s, reset after a run of 60 s. The file has no field for it.
- **Forwarding.** Init turns on `net.ipv4.ip_forward` and `net.ipv6.conf.all.forwarding` for the families `network` names, before it starts mvmd, since mvmd never sets it and refuses to start when a served family has it off (garamsh/mvmd#311).
- **naru knows no service's internals.** Name, binary and arguments come from this file.

## Rationale

- [ADR 0005](./adr/0005-configuration-is-a-yaml-file-on-a-labelled-ext4-disk-in-the-shape-of-gaons-resources.md): why a disk and not `fw_cfg`, why ext4, why gaon's shape, and the signs it was wrong.
- [ADR 0004](./adr/0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md): why P0 has no node API.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): the forwarding obligation, and the other facts mvmd needs from a Node's configuration.

## Open questions

- **The node API.** Its shape, and whether it accepts a `NodeConfig`, is not decided.
- **The node API's PKI.** How a caller is authenticated, and how a first Node accepts one, is not decided.
- **Fields beyond P0.** The prefix per family, IPv4 gateway and resolvers that mvmd's Node configuration needs (mvmd ADR 0011 Consequences), and the host network settings gaon will need for Nodes to connect, are not decided.
- **Configuration integrity.** Whether and how the file is signed is not decided.
