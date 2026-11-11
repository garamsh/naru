# ADR 0012: gaon runs on the Nodes with etcd on the data disk, certificates carried and not issued, and a host forward to the API server

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#19, part of goal garamsh/naru#25. It supersedes no ADR. The gaon facts below are read at garamsh/gaon commit `cd05f7ed59b62c43279e13d0cb3b3821c93abcc2` (`docs/architecture/deployment.md`, `apiserver.md`, `node-agent.md` and the `main.go` of each service), and the etcd facts at garamsh's pin of etcd-io/etcd, tag v3.6.16, commit `0b065e61c679ea81f6862bb24c9815f7fc673966`.

## Context

- **gaon names what it needs from naru.** `deployment.md` at the commit above says that every gaon service takes argv plus files (only the Node Agent reads `GAON_*` variables, as a fallback), that etcd is on the API server's loopback and dialled over plain HTTP, that etcd's data persists across a reboot, and that in the first milestone an operator or tooling issues the certificates (a CA, the API server's pair and one client pair per caller) and naru delivers them as files in the Node's configuration, a Node Agent's certificate having its Node's name as CN.
- **The data disk is the one persistent place.** The root is a tmpfs ([init.md](../init.md)); the data disk (label `NARUDATA`, ext4 with `verity`) is mounted at `/var/lib/mvmd`, where mvmd's state directory is `/var/lib/mvmd/state`. A Node that reboots loses everything but it.
- **gaon's end-to-end test (garamsh/gaon#9) needs to reach the API server from the host,** and the QEMU tests of naru run as the user, with no root, no bridge and no tap ([image.md](../image.md)). The Nodes' only network is a multicast socket netdev between QEMU processes, which the host does not join.
- **naru has no control plane** ([ADR 0002](./0002-gaon-depends-on-naru-one-way-through-the-node-api-and-naru-has-no-control-plane.md)): it starts services and holds no authorization system.

## Decision

**The image carries gaon's four binaries and etcd and the configuration starts them; etcd's data directory is `/var/lib/mvmd/etcd` on the data disk; the certificates are issued by test tooling and carried as files under `/run/gaon`; and the test host reaches the API server through a second, user-mode NIC with a host forward on the loopback.**

1. **Binaries.** `gaon-node-agent`, `gaon-apiserver`, `gaon-scheduler` and `gaon-controllers` are built with `CGO_ENABLED=0 -trimpath` from the gaon commit `image/gaon/pin.env` names, fetched with git and checked against that object name. etcd is built from source, the server binary only, from the commit `image/etcd/pin.env` names, fetched with git and checked against that object name; the build fails unless `etcd --version` reports the pinned version. Each is installed in `/bin`, and the initramfs build fails unless each is static. Each build sets `GOTOOLCHAIN` to the toolchain its own `go.work` (gaon) or `server/go.mod` (etcd) names, which Go's toolchain download supplies when the host has another, so that the bytes do not depend on the host's Go; `make image-repro` compares all five.
2. **Services.** Every Node runs `gaon-node-agent` with `--api-server`, `--node-name`, `--mvmd-socket` and the three TLS files. The Node with `spec.controlPlane` also runs, marked `controlPlane: true`, `etcd` (client URL `http://127.0.0.1:2379`, peer URL `http://127.0.0.1:2380`, one member, `--data-dir /var/lib/mvmd/etcd`), `gaon-apiserver` (`--listen 0.0.0.0:6443`, mutual TLS), `gaon-scheduler` and `gaon-controllers`, each with a client certificate of its own. No variable is set for any of them.
3. **Order.** In file order: mvmd, etcd, the API server, the scheduler, the controllers, the Node Agent, so each starts after what it dials. Init does not wait for a service to be ready, so the order is a courtesy: the API server exits when it cannot reach etcd and init restarts it after the backoff ([init.md](../init.md)), and the Node Agent retries every resync period. Init is unchanged.
4. **etcd's data is on the data disk, as a sibling of mvmd's state:** `/var/lib/mvmd/etcd` beside `/var/lib/mvmd/state` and `inputs`. Init, the mount and `config.md` are unchanged: etcd creates its directory, and the data disk's mount point keeps the name it has. mvmd's `supervision.dir` stays unset.
5. **The certificates are issued by the test tooling and carried by naru.** `tooling/qemu/internal/pki` makes a CA, the API server's serving certificate (IP SAN: the control-plane Node's address on the Nodes' network and `127.0.0.1`, the host forward's), one client certificate per Node with the Node's name as CN, one each for the scheduler and the controllers, and one for the test host. The files go in each Node's `spec.files` under `/run/gaon/`: the CA and the Node's agent pair on every Node, and on the control-plane Node the pairs of the API server, the scheduler and the controllers. Keys are mode `0600`. naru issues nothing at boot and holds no CA.
6. **The host path is a second NIC.** The control-plane Node gets a second `virtio-net-pci` on `-netdev user,restrict=on` with `hostfwd=tcp:127.0.0.1:<port>-10.0.2.15:6443`, the port chosen free per cluster; its `network-config` gives that NIC `10.0.2.15/24` and no gateway, so the default route stays on the Nodes' network. The host reaches the API server at `127.0.0.1:<port>` with the test host's client certificate. `restrict=on` leaves the guest no route to the host's network. The API server listens on `0.0.0.0` because a forwarded connection arrives on the second NIC's address.

## Consequences

- **A Node of the cluster runs gaon from the image alone,** with the four binaries and etcd of the pinned commits; moving a pin is the PM's, in a pull request that rebuilds and runs the tests.
- **The API server accepts connections on every interface of its Node,** the Nodes' network and, in the QEMU tests, the second NIC. It requires a client certificate the CA signed on each, so the exposure is that of its TLS configuration and not of its address; a Node whose network is not private should name an address in a real deployment's `--listen`.
- **The private keys are in the NoCloud drive** of each Node in the clear, as `spec.files` content, and on the tmpfs `/run/gaon`. They are a test CA's, valid for 48 hours. A real deployment's keys are the operator's to issue and protect; how they are renewed is gaon's open question, not decided here.
- **The initramfs is larger,** and so is the memory of the tmpfs root: the five binaries are about 70 MB uncompressed (see image.md for the measured figures), and each Node pays it whether or not it runs the control plane.
- **A cluster has one control-plane Node and one etcd member;** losing node-1's data disk loses every resource. gaon's own decision (its ADR 0004) is one member.
- **The test CA and the Node's second NIC exist only in the QEMU tests.** The release's example user-data stays as it is until garamsh/naru#28.
- **Go's toolchain download is a build dependency** of the image on a host whose Go is older than the pinned toolchains; a host with the same toolchain builds without downloading.

## Signs the decision was wrong

- A control-plane Node that reboots and comes back without a resource written before: the data directory is not on a disk that persists, or etcd was started on another directory.
- etcd failing for lack of space on the data disk in a test that also runs Machines: the disk is too small to share, and etcd needs its own.
- A need to reach the API server from a second host with no forward: the second NIC is the wrong form, and a routed address on the Nodes' network, with the serving certificate naming it, replaces it.
- A change to init to start etcd earlier, later or only when ready: the order is not a courtesy.
- gaon issuing its own certificates (its end state): naru's carriage becomes a bootstrap file and this item narrows to it.
- Two builds of the image on one host with different bytes for a gaon or etcd binary: the toolchain is not as pinned as stated.
