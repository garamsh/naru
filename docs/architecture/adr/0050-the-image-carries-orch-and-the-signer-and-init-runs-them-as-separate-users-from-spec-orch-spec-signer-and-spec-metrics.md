# ADR 0050: the image carries orch and the signer, and init runs them as separate users from spec.orch, spec.signer and spec.metrics

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#121, garamsh/naru#117 and garamsh/naru#118, and the open initrd item of garamsh/naru#116 (plan: garamsh/naru#113). It extends [ADR 0040](./0040-naru-signer-alone-holds-the-authority-keys-in-a-private-directory-and-issues-certificates-over-a-unix-socket-against-signed-requests-and-a-recovery-key-opens-the-bundle-that-restores-them.md) (the signer's directory and socket), [ADR 0045](./0045-each-network-of-a-host-is-closed-by-default-by-an-nftables-table-hostd-writes-and-wireguard-carries-the-networks-between-hosts.md) (hostd's ruleset and tunnel) and [ADR 0039](./0039-init-dissolves-into-lib-init-called-by-host-and-node-and-controlplane-into-lib-vip-and-node-and-a-check-fails-an-import-between-units.md) by reference: their bodies are not edited, and what they say holds with this decision added. It supersedes nothing.

## Context

- **ADR 0040 left the signer's place open.** Its user, its socket's group and its place in the image were not decided, and the image did not carry it. Orch is not carried either, so no deployment can run the control plane from the image.
- **Orch must not read the signer's keys.** The signer exists so that orch cannot make an identity. That holds only if the two run as different users and the signer's directory is private to its user.
- **Init supervised services as root.** It had no way to run a service as another user, to make a directory for it, to give a file an owner, or to run a command once before the service starts. The signer needs all four: its first start runs `naru-signer init`, a replica's runs `join-key` and `join`.
- **Hostd and the kernel.** Hostd's ruleset counts what it drops and its hosts reach each other over WireGuard ([ADR 0045](./0045-each-network-of-a-host-is-closed-by-default-by-an-nftables-table-hostd-writes-and-wireguard-carries-the-networks-between-hosts.md)); the host kernel had neither option. Hostd also takes its management prefix, egress interface and tunnel port, and enrolls with orch, from flags that no configuration wrote.
- **The node initrd had the host's `/init`.** The image had one `/init`, built from `host/cmd/naru-init`; the node's own `node/cmd/naru-init` was not installed.
- **Nothing was scraped.** Init, hostd, orch and the signer had no common place to serve metrics from.

## Decision

1. **The image carries `naru-orchd` and `naru-signer`** (built from `orch/cmd/naru-orchd` and `host/cmd/naru-signer`, `CGO_ENABLED=0`) in `/bin`. The host image's `/init` is `host/cmd/naru-init`; the node initrd's `/init` is `node/cmd/naru-init`, so each carries the init of its unit. The host kernel adds `CONFIG_WIREGUARD=y` and `CONFIG_NFT_COUNTER=y`; the guest node kernel already has WireGuard.
2. **Three sections of the configuration run the daemons.** Init derives the service, its files and its flags from them; `spec.services` may not name them.
   - `spec.signer`: `peerListen` and `peerNames` (both or neither) for the peer listener, and `bootstrap {recoveryPublicKey, rootCSR}` **or** `replica`. The two are exclusive. A replica's first start runs `naru-signer join-key`, which prints its public join key on the console; once root has authorised that key (`naructl signer authorise-replica --host-id <metadata.name>`), `replica.join {peer, peerCA, authorisation}` is added by a patch and init runs `naru-signer join` once. Neither step runs again while the signer's directory holds authorities.
   - `spec.orch`: `peerURL`, `clientURL`, `initialCluster` (member name to peer URL, this member's among them), optional `name` (`metadata.name`), `initialClusterState`, `listen`, `sans`, and the PEM `certificate`, `ca` and `revocationPublicKey`, an optional `recoveryPublicKey`, and `restore` (default false), which gives orchd `--restore`: with an empty data directory it waits for root to `PUT /v1/restore` an etcd snapshot. It needs `spec.signer` on the same Node. Orch's private key is a `spec.files` entry at `/run/naru-orchd/orch.key`, mode 0600, owned by 6002, so `GET /v1/config` redacts it; the section has no field for it.
   - `spec.host.network {managementPrefix, egressInterface, tunnelPort}` writes hostd's `--management-prefix`, `--egress-interface` and `--tunnel-port`; `spec.host.orch {url, caSHA256, enrollKey}` writes `--orch-url`, `--orch-ca-sha256` and `--enroll-key-file`. The key is written 0600, root's, to `/run/naru-hostd/enroll.key`, and `GET /v1/config` redacts `enrollKey`. Both need the `naru-hostd` service. Hostd takes no host address: `--host-addrs` is accepted and ignored.
3. **Separate users.** The signer runs as 6001:6001 with its directory `/var/lib/naru/signer` mode 0700 owned by 6001; orch runs as 6002:6002 with `/var/lib/naru/orch` 0700, and its supplementary group is 6001, which is the group of the signer's socket. Orch's user cannot read the signer's directory.
4. **A service has a user, directories, and setup steps; a file has an owner.** `spec.services[].user {uid, gid, groups}` is the credential of the child; `dirs [{path, mode, uid, gid}]` are made and owned before the start; `setup` is a list of `{args, unless}` steps of the service's own binary, run in order as the service's user before the service starts. A step does not run while any path of its `unless` exists, and a step that fails stops the start. `spec.files[].owner {uid, gid}` is applied and compared, so a change of owner is an apply.
5. **Metrics.** `spec.metrics.listen` is an IP address. Init serves `/metrics` on it at port 9100, and passes `--metrics-addr` to hostd (9101), orch (9102) and the signer (9103). Changing the listener needs a reboot. Orch exports `naru_orchd_info` and the Go and process collectors; the signer adds `naru_signer_http_requests_total{listener,method,code}`.
6. **Reserved.** The services `naru-orchd` and `naru-signer`; hostd's `--management-prefix`, `--egress-interface`, `--tunnel-port`, `--metrics-addr`, `--orch-url`, `--orch-ca-sha256` and `--enroll-key-file`; files under `/run/naru-orchd/` other than `orch.key`, under `/run/naru-signer/`, and `/run/naru-hostd/enroll.key`. Init derives them and removes them when their section goes.
7. **`naructl gen node` writes the sections** from flags, composing through `naructl/internal/layout`. Orch's private key and the enrollment key are read from files and written into the user data (0600), and are never printed.

## Consequences

- A Node that sets `spec.signer` and `spec.orch` runs a control plane member from the image alone. Its keys are in its user data: orch's key, the enrollment key and the replica's authorisation are as secret as the user data, and orch's and the enrollment key are redacted from `GET /v1/config`.
- A replica takes two applies, with root's action between them. `join-key`'s console line is base64 of the raw 32 bytes, so the operator decodes it before `authorise-replica` unless naructl accepts base64.
- `peerCA` is part of `replica.join` because `naru-signer join --peer-ca` is required and a replica Node need not run orch, so `spec.orch.ca` is not always there.
- `spec.host.network.hostAddrs` is gone from the sections; an old document that sets it is refused as an unknown field.
- Orch's host-client flags (`--host-client-cert`, `--host-client-key`, `--host-ca`) are not written by any section.

## Signs the decision was wrong

- A third daemon needs a user and the same four fields: the generic form holds; if each daemon needs a different launch path, a launcher per daemon would have been right.
- Two users prove not enough, for instance orch needing a file only the signer's group may read beyond the socket.
- The reboot a metrics-listener change needs is felt: the listeners would have to move without one.

## Note of 2026-11-14: how orch reaches the hosts

This note appends to the decision above and supersedes the Consequences line that says orch's host-client flags are written by no section.

- Orch presents its own certificate, the pair of `spec.orch`, to the hosts, not a separate host-client pair. Init writes `--host-client-cert=/run/naru-orchd/orch.crt` and `--host-client-key=/run/naru-orchd/orch.key`, and `--host-ca=/run/naru-orchd/host-ca.crt` from the new optional `spec.orch.hostCA` (PEM; without it orchd trusts `--ca`). The file is mode 0644 and restarts orch.
- naru-hostd's role comes from the Organization `naru:host-provisioner` or `naru:host-reader`, and now also from `lib/identity`: a certificate whose one OrganizationalUnit is `orch` is a provisioner. Only that unit is read; a certificate that carries `orch` beside another unit has no role. This is the change the Consequences above name ("until the host API takes `lib/identity`'s roles").
- The hosts' client trust is a bundle. `naructl gen node --host-client-ca` appends the authority orch's certificate chains to the host authority in `/run/naru-hostd/client-ca.crt`; `--orch-host-ca` writes `spec.orch.hostCA`.
- A certificate with the `orch` unit is also what the signer's replicas present on the replication channel. A host that trusts the authority those chain to takes such a certificate as a provisioner, because the host reads only the unit.

## Note of 2026-11-14: a member that joins a running one

This note appends to the decision above and to the section it names under decision 2, `spec.orch`. It follows [ADR 0053](./0053-a-member-started-with-restore-serves-only-a-snapshot-upload-and-the-operator-sends-it-the-plain-snapshot-and-the-other-members-join-it-as-new-members.md), whose `naru-orchd --join` had no section to write it from.

- **`spec.orch.join`** is the client URL, `https://<host>:<port>`, of a running member. Init writes orchd's `--join <url>` and `--initial-cluster-state=existing`, and no `--initial-cluster`: the member adds itself to that cluster at its first start and takes the initial cluster from the one it answers, so the section's `initialCluster` is empty. A Node whose `spec.orch` has `join` and an `initialCluster`, a `restore`, or the state `new` is refused when its document is parsed.
- **Why `initialCluster` is refused, not ignored.** A value the member does not use is one an operator edits and expects to matter; the member that joins takes the cluster etcd returns, which has the members the cluster holds when it joins, not the members the operator wrote down some time before.
- **Why `restore` is refused.** A member that restores starts a cluster of itself alone and the others join it; one member cannot do both at its first start.
- **`naructl gen node --orch-join <url>`** writes the field, in place of `--orch-member`.
