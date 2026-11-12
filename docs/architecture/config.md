# Config

How a Node's configuration and network reach init in P0, and the shape of the files.

Claims about garamsh/gaon hold at commit `cd05f7ed59b62c43279e13d0cb3b3821c93abcc2`, and about garamsh/mvmd at `b68db37c093b764d40a3930618f47041178253f7`; they promise nothing about later versions. They were read at those commits when they replaced `d643b1cb6a6cc01ea587dd7c967bdfd07280fe0b` and `3b37892f303a4d4dbd8e5360906d0f9c036b562e`: gaon's `resource-api.md` §Common rules and mvmd's `runtime.md` §Network and `daemon/internal/app/config.go`.

## Current decisions

- **Delivery.** A cloud-init NoCloud drive: a block device with an ISO 9660 volume labelled `cidata` (either case), the form Proxmox's cloud-init drive takes. Init mounts it read-only and reads two files: `user-data`, which is `naru.yaml`, and `network-config`. `meta-data` is not read. It is not `fw_cfg`, and no ext4 `NARUCFG` disk is read.
- **Shape.** `apiVersion: naru.garam.sh/v1alpha1`, `kind: NodeConfig`, `metadata.name` the node name, and `spec`. There is no `status`. The shape follows gaon's resource rule, "Every resource has `apiVersion`, `kind`, `metadata`, `spec` and `status`" (gaon `docs/architecture/resource-api.md` §Common rules), minus `status`, which a file has no observer for.
- **`spec` holds:**

  | Field | Meaning |
  |---|---|
  | `services` | a list; each has a name, a binary path, args, env, and `controlPlane: bool`, which marks a service started only on the node that runs the control plane |
  | `controlPlane` | `bool`; whether this node runs the control plane |
  | `dataDisk` | `device` or `label` (exactly one), and `mountPoint` |
  | `network` | `forwarding`: the families, `ipv4` and `ipv6`, for which forwarding is turned on |
| `files` | a list of files init writes before any service starts; each has `path` (absolute, under `/run/`, and not `/run/naru` or below it), `mode` (octal, at most `0777`; `0644` when absent) and `content`. No path is another's directory |
| `pki` | optional. `authorities`: each has `name`, `certificate` (PEM), `key` (PEM, optional), `certificatePath` and `keyPath` (optional, and only with a `key`). `certificates`: each has `name`, `authority` (one that carries a `key`), `certificatePath`, `keyPath`, `commonName`, `organization` (optional), `usage` (`server` or `client`), `dnsNames`, `ipAddresses`, `nodeAddresses` (adds the addresses of `network-config`, and `127.0.0.1` to a server certificate), `validity` (a duration from `1h` to `87600h`, `720h` when absent; a certificate lives no longer than its authority's certificate) and `restart` (services of `spec.services` that init restarts when it renews the certificate). Every path follows the rules of a `files` path and is no file's of `files`, nor another's of `pki`. Init issues the certificates at boot and renews them ([init.md](./init.md), [ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md)) |

- **The network is `network-config`**, in the version 1 form Proxmox writes (qemu-server `src/PVE/QemuServer/Cloudinit.pm`, at commit `b0965eeba61248046f803a4995112f0ab8523843`). Init accepts only `version: 1`; `physical` cards, each with a `mac_address` or a `name` (the MAC wins when both are given, and init does not rename a link), a static IPv4 subnet (`address` with a dotted `netmask`, or in CIDR form) and at most one `gateway`, which lies inside the prefix; and one `nameserver` entry with `address` and `search` lists. It refuses DHCP, IPv6, bonds, VLANs, bridges, routes, an unknown field and a malformed value. Init sets each link up, adds its address and the default route through netlink, and writes the resolvers to `/run/naru/resolv.conf`, which `/etc/resolv.conf` links to.
- **Init validates before it acts.** An unknown `apiVersion` or `kind`, a field the shape does not name, a value that fails validation, or a file that does not parse, stops init before any service starts; the error names the field. The same holds for a `network-config` that is malformed or asks for what init does not apply, and for a drive that is missing, has no `user-data` or has no `network-config`. What init does next is in [init.md](./init.md).
- **Restart backoff is fixed in P0.** A service that exits is restarted after 1 s, doubling to a cap of 30 s, reset after a run of 60 s. The file has no field for it.
- **Forwarding.** Init turns on `net.ipv4.ip_forward` and `net.ipv6.conf.all.forwarding` for the families `network` names, before it starts mvmd, since mvmd never sets it and refuses to start when a served family has it off (garamsh/mvmd#311).
- **mvmd's `network` section is required.** mvmd refuses, at load, a Firecracker or Cloud Hypervisor configuration without it (mvmd `docs/architecture/runtime.md` §Network, "A real Driver needs the `network` section"; `daemon/internal/app/config.go`, `Config.check`). The Node configuration tooling writes `network.ipv4_prefix`, `ipv4_gateway` and `resolvers` (`node.Config.MvmdJSON`), and `spec.network.forwarding` turns forwarding on, so both of mvmd's conditions hold.
- **mvmd's `supervision.dir` is left empty.** mvmd refuses one that is the state directory, an ancestor of it, or inside it other than at `machines`; empty means `machines` under `state_dir` (`daemon/internal/app/config.go`, `sharesStateDir`), which `node.Config.MvmdJSON` relies on.
- **naru knows no service's internals.** Name, binary and arguments come from this file.
- **gaon's certificates are `files`, or are issued from `spec.pki`.** A Node that carries no CA key gets its certificates as `files`, as below. A Node whose `spec.pki` carries the CA and its key gets the pairs init issues at boot under `/run/gaon/`, in the same places, and the services name them with the same flags ([ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md), which supersedes item 5 of ADR 0012). For `files`: a Node's CA, its agent's pair and, on the control-plane Node, the API server's, the scheduler's and the controllers' pairs are entries of `spec.files` under `/run/gaon/`, which the gaon services name with their `--tls-*` flags, as gaon's `deployment.md` §Certificates and §Config shape ask. The files' content is the tooling's input; init writes it and knows nothing of it ([ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md)).

## Operator tooling: naructl

`naructl` is the operator's tool for a gaon cluster on naru Nodes. It writes the CA, each Node's user-data and `network-config`, and the operator's client certificate pair; it owns the gaon Node layout (services, `spec.pki`, the worker's files, mvmd's configuration, the data disk's paths), which `tooling/qemu` runs as a binary and does not copy ([ADR 0015](./adr/0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md)). It is a static linux/amd64 binary, the release asset `naructl-<version>-linux-amd64`, and every file it writes is one `naru-init -validate` accepts. The example below is the release's example deployment, with `guest.json` the release's `naru-guest-<version>.json` and `$FIRECRACKER` the `firecrackerSha256` of its `naru-inputs-<version>.json`:

```sh
naructl gen ca --out ca                        # ca/ca.pem, ca/ca.key
naructl gen node --ca ca --name node-1 --control-plane \
  --address 192.168.0.40/24 --mac 02:00:00:00:03:e9 --gateway 192.168.0.1 --nameserver 1.1.1.1 \
  --api-server 192.168.0.40 --machine-prefix 10.200.1.0/24 \
  --guest guest.json --firecracker-sha256 "$FIRECRACKER" \
  --out node-1.user-data --network-config node-1.network-config
naructl gen node --ca ca --name node-2 \
  --address 192.168.0.61/24 --mac 02:00:00:00:03:f4 --gateway 192.168.0.1 --nameserver 1.1.1.1 \
  --api-server 192.168.0.40 --machine-prefix 10.200.2.0/24 \
  --guest guest.json --firecracker-sha256 "$FIRECRACKER" \
  --out node-2.user-data --network-config node-2.network-config
naructl gen client --ca ca --cn alice --out client   # client/client.pem, client/client.key
```

- **`gen ca --out <dir>`** writes `ca.pem` and `ca.key` (ECDSA P-256, valid ten years; the key `0600`) and refuses to replace an existing `ca.key`.
- **`gen node`** writes the Node's `user-data` (`--out`, mode `0600`) and, with `--network-config`, its `network-config`. On the control-plane Node (`--control-plane`) `spec.pki` carries the CA and its key and init issues the API server's, scheduler's, controllers' and agent's pairs at boot ([ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md)); a worker's user-data carries `ca.pem` and its own agent pair, whose CN is the Node's name, signed by the CA (certificates valid one year, never past the CA's). `--pairs-as-files` makes the control-plane Node carry its pairs as files and no CA key instead, which is the form the release's test user-data takes. `--api-server` is the address every agent dials and the API server's serving certificate names; `--serving-address` (repeatable) adds more. `--binary <service>=<path>`, `--disable <service>`, `--extra-nic <mac>=<cidr>` and `--search` are the other flags; `naructl gen node -h` lists all. `--name` must be a DNS label, since it is the Node's CN, its etcd name and its `metadata.name`. A flag given without what makes it apply is refused: `--mac`, `--gateway`, `--nameserver`, `--search` and `--extra-nic` need `--network-config`, and `--serving-address` and `--pairs-as-files` need `--control-plane`. A file already at `--out`, or at a key's path, is replaced by one with the mode named here, never written into with the old file's mode. A CA whose certificate has expired is refused.
- **The registry serves the data disk's test image.** A Node runs `registry` by default, and mvmd trusts its certificate through `SSL_CERT_FILE`, so the data disk's image is pullable by digest; the release's `naru-data-<version>.qcow2` carries it. `--disable registry` leaves both out, so an operator who pulls from elsewhere drops the registry and mvmd's `SSL_CERT_FILE` together.
- **`gen client`** writes `client.pem` and `client.key` for an operator, CN as given, signed by the CA, for gaon's CLI (`--tls-ca ca/ca.pem --tls-cert client/client.pem --tls-key client/client.key`).
- **The CA key goes only into the control-plane Node's user-data and the operator's secret store.** `naructl` writes it to `ca.key` and to `spec.pki` of the control-plane Node, and nowhere else: a worker's user-data holds none, and a release asset never does. Whoever holds `ca.key` can issue a certificate the API server accepts, so keep it out of version control, and treat the control-plane Node's user-data (and the NoCloud drive it is on) as holding it. The release's `naru-node-<n>.test.yaml` are different: they are made with `--pairs-as-files` from a CA that is discarded, so they hold no CA key ([image.md](./image.md#release-assets)).

## Rationale

- [ADR 0015](./adr/0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md): why one tool owns the gaon Node layout, and why `tooling/qemu` runs it as a binary.
- [ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md): why `spec.pki` carries authorities and certificates, the validation, and the signs it was wrong.
- [ADR 0008](./adr/0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md): why a NoCloud drive, why `network-config` is read strictly, why the files field, and the signs it was wrong. It supersedes ADR 0005's delivery.
- [ADR 0005](./adr/0005-configuration-is-a-yaml-file-on-a-labelled-ext4-disk-in-the-shape-of-gaons-resources.md): why gaon's shape and the forwarding duty; its delivery by an ext4 disk is superseded.
- [ADR 0004](./adr/0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md): why P0 has no node API.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): the forwarding obligation, and the other facts mvmd needs from a Node's configuration.

## Open questions

- **The node API.** Its shape, and whether it accepts a `NodeConfig`, is not decided.
- **The node API's PKI.** How a caller is authenticated, and how a first Node accepts one, is not decided.
- **Fields beyond P0.** DHCP, IPv6, several addresses per card and routes in `network-config`, and the host network settings gaon will need for Nodes to connect, are not decided.
- **Configuration integrity.** Whether and how the file is signed is not decided.
