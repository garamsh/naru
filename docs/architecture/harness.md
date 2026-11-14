# Harness

The public contract of `naru-cluster`, the command an external harness runs to boot naru Nodes under QEMU: its flags, the `cluster.json` it writes, and what a harness may rely on. A change to anything on this page is a breaking change of a public surface.

## Current decisions

- **The command** is `github.com/garamsh/naru/tooling/qemu/cmd/naru-cluster`. A harness builds it and then runs the binary: `GOBIN=<dir> go install github.com/garamsh/naru/tooling/qemu/cmd/naru-cluster@<commit>`, then `<dir>/naru-cluster ...`, from any directory. It needs nothing of a naru checkout, and it builds no program. `go run …@<commit>` is not a supported form: `go run` does not forward SIGINT or SIGTERM to the program, so a signal that stops `go run` leaves the program and its QEMU processes running.
- **It boots from a release.** `-release` names a directory with the assets `make release` writes. It reads `naru-<version>.qcow2`, `naru-data-<version>.qcow2`, `naru-guest-<version>.json`, `naru-inputs-<version>.json` and `naructl-<version>-linux-amd64`; the version is the one in the guest manifest's name, and the directory must hold exactly one. The `naructl` asset is run to write every Node's user-data and the client pair ([config.md](./config.md#operator-tooling-naructl)); a release without it is refused, naming it, as ADR 0014 did for `naru-inputs`: every release made before `naructl` is one. The same `naructl` is asked for its topology (`naructl topology`, [ADR 0019](./adr/0019-the-gaon-topology-and-a-nodes-credential-mode-are-defined-once-in-naructls-layout-and-the-harness-reads-the-topology-from-naructl-topology.md)): the services that exist, which of them only node-1 runs, which signs the agents' requests, and which serve the API and register a Node. `naru-cluster` keeps no list of its own, and a `naructl` with no `topology` command (the one in `v0.3.0-rc.1` and `v0.3.0`) is refused, saying so. The flags and `cluster.json` are unchanged by that, and `version` stays. It converts the two disks to raw files under `-dir` and reads the data disk's `inputs/` tree with `debugfs`. It does not read `naru-upgrade-<version>.tar`, which a release carries from v0.5.0 ([ADR 0020](./adr/0020-a-node-boots-one-kernel-file-per-slot-of-the-esp-with-the-initramfs-embedded-and-a-trial-slot-is-reached-once-by-bootnext.md)): a release directory with it is booted as before. The boot disk of that release has two kernel slots and no trial entry, and boots the default.
- **It runs gaon on the Nodes** as the QEMU tests do ([image.md](./image.md), [ADR 0012](./adr/0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md)): mvmd, the registry and the Node Agent on every Node, and etcd, the API server, the scheduler and the controllers on node-1, which has the second NIC and the host forward.
- **The QEMU tests boot a third Node.** `ClusterSpec.ControlPlanes: 3` makes every Node a control-plane Node: each gets the second NIC and its own host forward to its API server, `--member node-i=10.78.0.i`, and the virtual address `10.78.0.100` is `--api-server`. Each Node has 3072 MiB and 2 vCPUs, as the two-Node tests; three fit the machine (about 21 GB available at the measurement). `ClusterSpec.EtcdProbeOn` appends `naru-etcdprobe` as a service of that Node, with an alias in that Node's MicroVM prefix and the pair `naructl gen etcd-client` wrote (passed as PEM in its arguments). `ClusterSpec.SerialSocket` gives every Node's serial port as the server socket `<node dir>/serial.sock` that nothing reads, in place of the `console.log` file (the Node's `Console()` is then empty), the shape of a Proxmox `serial0: socket`; `TestControlPlane_ThreeNodesWhoseSerialSocketNobodyReadsAnswerWithinASecondAndTheHolderNeverReleasesTheAddress` boots three control-plane Nodes so, probes each 5 times a second for 120 s starting 90 s after its kept console log is full, and reads node-1's logs through its Node API: no handshake over 1 s, no new `released` line, no `TLS handshake error` ([ADR 0036](./adr/0036-a-re-print-carries-at-most-the-newest-16-kib-of-the-console-log-and-the-api-servers-liveness-probe-is-a-tls-request-that-it-can-answer.md)). `naru-cluster` boots one control-plane Node as before ([ADR 0024](./adr/0024-the-control-plane-is-three-etcd-members-with-tls-on-both-ports-and-a-virtual-address-held-by-naru-vip-behind-a-fence-on-etcds-ports.md)).
- **The QEMU tests reach a microVM's address through a route.** A microVM's address is on its host's Machine prefix behind the Node's bridge, and QEMU's user-mode forward can only target an address the Node itself owns. `ClusterSpec.Host` therefore also runs `naru-mvmroute` on node-1 (`tooling/qemu/cmd/naru-mvmroute`), which listens on the second NIC at `10.0.2.15:7480` and dials only inside node-1's Machine prefix, and forwards a loopback port of the test host to it (`HostAPI.Route`). `HostAPI.RouteTo(ctx, "address:port")` opens a loopback listener on the test host for one target: the first line the route reads is the target, it answers `ok` or `refused: <why>`, and then it copies both ways, so the TCP service of the microVM, such as its Node API on 50000 or its API server on 6443, is reached at the listener's address with the service's own TLS unchanged. The route is a test tool and no part of the release or of `naru-cluster`'s contract. `ClusterSpec.MemoryMiB` sizes every Node, for a host that runs a Kubernetes Node as a microVM. The calls `naructl` has no command for (snapshot, resume, networks) go through `lib/hostapi`'s client over a `naructl` client file (`tooling/qemu/internal/hostcli`).
- **The QEMU tests run a fleet of hosts.** This is the tests' own surface, not `naru-cluster`'s.
  - `ClusterSpec.Hosts` lists the Nodes that run `naru-hostd` (each has its own `10.77.<n>.0/24` Machine prefix and a `HostAPI` with `Index`), `ClusterSpec.Forward` the guest ports of every Node's second NIC that get a loopback port on the test host (`Cluster.Forwards[node][port]`), and `ClusterSpec.Capture` records the multicast network the Nodes share to a pcap file (`Node.CapturePath`) which `tooling/qemu/internal/pcap` reads and filters by the two hosts' addresses.
  - A scenario edits the Node's user-data after its first boot with `Node.ReplaceUserData` and `Node.Reboot`, and has a control host lose its disk with `Node.ResetData` (a new data disk with only the inputs tree). `internal/seed` sets, appends to and deletes fields of `spec` (`Set`, `Append`, `Delete`) for what naructl writes no flag for: `spec.signer`, `spec.orch`, `spec.metrics`, `spec.host.network`, `spec.host.orch`.
  - The scenarios do the operator's part with naructl and throwaway keys: `orch init`, `orch csr`, `operator request|person|add|revoke`, `signer authorise-replica|bundle|restore`. Root's certificate and the revocation key are read from the first signer's console (`internal/consolepem`); the signer's socket on each control Node is reached by `naru-sockproxy` in the guest and `internal/unixfwd` on the test host. The join, orch and restore wiring is the product's (`spec.signer.replica.join`, `spec.host.orch`, `spec.orch.restore`); the harness adds no service for it.
  - Four programs of the Inputs tree observe what a test cannot: `naru-entry probe` (a microVM that dials a TCP address, sends a datagram or a datagram with a forged source and lists what it received), `naru-nftprobe` (the host's nft counters on the console), `naru-keyprobe` (scans the file systems for the private key of a given public key outside the signer's directory) and `naru-sockproxy`. `naru-keyprobe` is run with a throwaway key planted as a positive control. What crosses the link between two hosts is read from the capture, and a payload marker is looked for in its raw bytes.
- **It does not expose the Node API.** `naru-cluster` writes no `spec.api`: its Nodes run with the Node API off, as the release's test user-data does ([ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)), and `cluster.json` names no client file or Node API address, so `version` stays 1. The QEMU tests reach the Node API through their own cluster option ([image.md](./image.md)); a harness that needs it is an open question below.
- **It keeps running until signalled** (SIGINT or SIGTERM), then stops every Node and exits 0.
- **A signal before `cluster.json` exists** also stops every Node started, at any point from the command's start, and exits with status 3 and the one-line reason `signalled before every Node was up`. Status 1 is any other failure. Whatever the way the command ends, including SIGKILL, no Node's QEMU process outlives it: each QEMU is started with the kernel's parent-death signal set to SIGKILL.

## Flags

| Flag | Meaning |
| --- | --- |
| `-release <dir>` | Required. The release directory. |
| `-dir <dir>` | Required. Where the command puts the Nodes' disks and serial logs, the client files and `cluster.json`. It must not exist or must be empty; any other content is an error naming the directory, because a second run on it would mix its inputs with the first's. It holds, per Node, `<node>/` with the disks, the serial log, `naru.yaml` and `network-config`; `guest.json`; `client/` and `pki/`, each with the client pair and the CA's certificate; and `cluster.json`. The cluster's CA directory is removed once the last Node's user-data is written, so no `ca.key` is left. With `-boot-pki` true (the default) node-1's `naru.yaml` carries the CA's key, which is a test CA's, so a harness that archives `-dir` archives it. |
| `-n <count>` | The number of Nodes, named `node-1` to `node-<count>`. Default 2. |
| `-gaon-bin <dir>` | Any of `gaon-apiserver`, `gaon-scheduler`, `gaon-controllers` and `gaon-node-agent` found in the directory is copied to every Node's data disk under `/var/lib/naru/inputs/gaon/`, and the service runs it in place of the image's. A binary not found leaves the image's. It is an error when the directory holds none of the four. Leaving a binary out does not disable the service. |
| `-inputs <dir>` | The directory's files are copied to `/var/lib/naru/inputs/extra/` on every Node. A symbolic link or any other file that is not a regular file or a directory is an error naming it. |
| `-disable <service>[,<service>...]` | The named services are left out of every Node's configuration. The names are the services the release's `naructl topology` lists (at this writing `registry`, `etcd`, `gaon-apiserver`, `gaon-scheduler`, `gaon-controllers` and `gaon-node-agent`); any other name is an error, refused once the release's `naructl` is read and before any disk is converted, and `mvmd` cannot be disabled. The signer, `gaon-controllers` at this writing, needs `-boot-pki=false`: with `-boot-pki` true it signs the agents' certificate requests, so `naru-cluster` refuses the pair before it starts a Node, with a one-line error naming `-boot-pki=false`. |
| `-boot-pki` | Default true: node-1 issues its certificates at boot from the CA its `spec.pki` carries ([ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md)). With it, every Node's agent holds a bootstrap token and is issued its pair by a signing request that `gaon-controllers` approves, so the gaon binaries a harness supplies must be the build that pin names ([ADR 0016](./adr/0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md)); the cluster's CA key is on node-1 for the controllers to sign with. `-disable gaon-controllers` is refused with it. False: every Node carries its pairs as files and no Node holds the CA key, and `-disable gaon-controllers` is allowed. `cluster.json`'s client pair is signed by the cluster's CA either way. |
| `-timeout <duration>` | How long the Nodes have to come up: one deadline, set once every Node's process has started, for every Node's services and for every Node's registering and becoming Ready together. Default 5m. |

The command logs to standard error, as `slog` text lines.

## cluster.json

`<dir>/cluster.json` is written once every Node is up, and not before: its presence says so. It is written whole and renamed into place, so a reader sees all of it or none. A Node is up when init has started each service its configuration names that the Node runs, and, unless `gaon-apiserver` or `gaon-node-agent` is disabled, every Node is registered in the API server and Ready.

```json
{
  "version": 1,
  "apiServer": { "address": "127.0.0.1:41233" },
  "pki": {
    "ca": "/abs/dir/pki/ca.pem",
    "clientCert": "/abs/dir/pki/client.pem",
    "clientKey": "/abs/dir/pki/client.key"
  },
  "nodes": [
    { "name": "node-1", "pid": 4242, "serialLog": "/abs/dir/node-1/console.log", "controlPlane": true },
    { "name": "node-2", "pid": 4243, "serialLog": "/abs/dir/node-2/console.log", "controlPlane": false }
  ],
  "images": [
    { "reference": "127.0.0.1:5000/test/entry@sha256:..." }
  ]
}
```

- **`version`** is an integer. It changes only when a documented field is removed, renamed or changes meaning, or the file's layout changes so that a reader of the old version misreads it. A field added is optional and leaves it as it is, so a reader ignores fields it does not know.
- **`apiServer.address`** is `127.0.0.1:<port>`, the host side of the forward to node-1's API server on port 6443. The serving certificate names `127.0.0.1`. The resource API's root is `https://<address>/apis/compute.gaon.garam.sh/v1alpha1`.
- **`pki`** names three files, all mode 0600, in PEM. `ca` is the CA that signed the API server's certificate and the Node Agents', the scheduler's and the controllers'. `clientCert` and `clientKey` are a client pair for the test host, signed by that CA, which the API server accepts.
- **`nodes`** is in order, node-1 first. `name` is the Node's name, `pid` the process ID of its QEMU process, `serialLog` the file its serial console is written to, and `controlPlane` its `spec.controlPlane`. The paths are absolute.
- **`images`** lists images the Nodes' mvmd can pull. `reference` is `repository@sha256:<digest>`, an image that every Node's registry serves (the registry listens on the Node's `127.0.0.1:5000`, which is the repository's host) and that mvmd pulls by digest. It is the image the QEMU tests' Machine runs.

## What a harness may rely on

- **The files in `cluster.json` exist and stay** while the command runs: the client pair works for the cluster the file describes, whichever of the two certificate paths `-boot-pki` selects.
- **Killing a Node's process** with `kill -KILL <pid>` is a power-off of that Node. The command logs a `node lost` line naming it, does not restart it, does not exit, and keeps the other Nodes running. A killed Node stays down for the rest of the run.
- **The command stops every Node it started** when it is signalled, and no Node outlives it however it ends. It does not clean `-dir`.
- **When the deadline passes** the command exits 1, naming the Node and the services it was waiting for and the last lines of that Node's console.
- **Services run the binaries `-gaon-bin` supplied**, from `/var/lib/naru/inputs/gaon/<name>`, and no other copy.
- **A harness may not rely on** the Nodes' addresses, the host port's value, the order of services starting, the content of the logs besides what is said here, the paths under `-dir` other than those `cluster.json` names, or the test image's content beyond its being pullable by digest.

## What the command needs

`/dev/kvm`, `qemu-system-x86_64`, `qemu-img`, the OVMF files at `/usr/share/OVMF/OVMF_CODE_4M.fd` and `OVMF_VARS_4M.fd`, and `/sbin/mkfs.ext4`, `/sbin/debugfs` and `/sbin/e2fsck` (`e2fsprogs`). It needs no root and no network beyond the loopback. Each Node takes about 3 GiB of memory.

## Rationale

[ADR 0014](./adr/0014-naru-cluster-is-a-public-command-that-boots-nodes-from-a-releases-assets-and-publishes-cluster-json.md): why the command reads a release and builds nothing, why the release gains `naru-inputs-<version>.json`, and why the output is a versioned file.

## Open questions

- **The Node API of a cluster `naru-cluster` boots.** `cluster.json` could carry a client file and the forward's address (optional fields, which leave `version` as it is) with a flag that has `naructl gen api-ca` write the authority. It is not done: no harness has asked for it, and the QEMU tests of the Node API run from the checkout with their own cluster option.
- **A way to pull another image into the Nodes' registry.** `-inputs` puts files on the data disk, and the registry serves the directory the release carries, so a harness that needs an image of its own has no supported way to add one.
- **Boot disks other than the release's.** The command reads only a release.
