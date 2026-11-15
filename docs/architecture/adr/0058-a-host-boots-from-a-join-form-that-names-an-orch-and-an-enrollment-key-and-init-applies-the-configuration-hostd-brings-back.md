# ADR 0058: a host boots from a join form that names an orch and an enrollment key, and init applies the configuration hostd brings back

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#164, part of plan garamsh/naru#113 and goal garamsh/naru#811. The direction is aurora#811 comment 6886769198 and its Amendment 1 (comment 6870869192), and the PM's brief for #164, recorded as written. The one mid-task addition, `spec.api.keyFile`, was directed by the PM.

## Context

- **A host's configuration is a whole `naru.yaml` today.** The operator writes services, files, the Node API pair and the network by hand ([config.md](../config.md)), so a host cannot be given to a cluster by naming the cluster.
- **Orch enrolls a host.** `naru-hostd --join` presents an enrollment key to an orch whose certificate chains to a pinned SHA-256, and writes the configuration the orch gave it ([ADR 0050](./0050-the-image-carries-orch-and-the-signer-and-init-runs-them-as-separate-users-from-spec-orch-spec-signer-and-spec-metrics.md)).
- **Init applies a configuration through one engine** with three modes and a base-SHA-256 rule ([ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-is-changed-by-merge-patch.md)).
- **Init starts no DHCP client** ([init.md](../init.md) §Current decisions), and a joining host has no address written by an operator.
- **The data disk is a precondition of every service** and is found by an ext4 label ([init.md](../init.md)).

## Decision

**A host's `user-data` may be a join form: an orch URL, the SHA-256 of its authority and an enrollment key. Init leases an address, prepares a data disk, runs `naru-hostd --join`, and applies the configuration it writes as a new revision.**

1. **The join form.** `spec.join` has `orch.url` (https), `orch.caSHA256` (64 hex digits) and `enrollKey` (`naru-ek-` and 64 hex digits). `metadata.name` is optional. A `spec` field beside `join` is refused, naming the field; a configuration that is not a join form with `spec.join` is refused.
2. **DHCPv4.** A client in `lib/init/dhcp4` runs DISCOVER, OFFER, REQUEST and ACK over a packet socket, with no libc.
   - A `network-config` interface of `type: dhcp4`, or no `network-config`, runs it; with no `network-config` the link is the first Ethernet link with a carrier, waited for up to 30 s.
   - A boot with no lease in 90 s is refused. After a NAK the client waits before it starts again.
   - The client renews for as long as the boot lasts: a unicast renewal at T1, a broadcast rebind at T2, and at expiry init removes the address and the default route and logs it.
   - Init writes `/run/naru/resolv.conf` from option 6, and uses option 42 as the time servers when there is no `spec.time`.
   - The lease's address is the Node's address for `spec.pki` and for any other reader, through one accessor. Certificates are not issued again when a renewal changes the address.
   - Tests use an in-process server behind a seam at the packet I/O.
3. **The data disk.** For a join form it is the disk whose ext4 label is `NARUDATA`; else the first blank whole disk other than the boot disk, which init formats and labels; else the boot is refused.
   - The label is `NARUDATA`, the label init already uses. The comment this ADR follows writes "naru-data"; the difference is raised in the pull request.
   - The boot disk is the one holding the partition whose PARTUUID is `cb4162e7-9d1e-48df-b937-f5792071b950`, the ESP.
   - A disk is blank when it has no partition table and no file system signature in its first and last MiB.
4. **The derived service.** `naru-hostd --join --orch-url <url> --orch-ca-sha256 <hex> --enroll-key /run/naru-hostd/enroll.key --host-key /var/lib/naru/hostd/host.key --join-config /var/lib/naru/join/config.yaml`.
   - The key is a derived `spec.files` entry, mode `0600`, owned by root. Hostd runs as root.
5. **Restart policy.** `spec.services[].restartPolicy` is `always` (the default) or `on-failure`. The join service is `on-failure`: no restart after an exit with status 0, a restart with backoff after any other exit. The service manager reports a service's exit through `Exited`.
6. **The handoff.** When the join service exits with status 0 and `/var/lib/naru/join/config.yaml` exists, init applies it as a whole configuration in `reboot` mode through `lib/init/apply`, removes the file once the store is written, logs `join configuration applied revision=<n>` and reboots.
   - A configuration with `spec.signer`, `spec.orch` or `spec.join`, one that does not parse and one the engine refuses are logged as `join configuration refused` with the reason. The file is renamed `config.yaml.refused` and the service is not started again before the next boot.
   - The stored revision keeps the host on its full configuration across reboots, because its base digest is the join form's (ADR 0018 item 4).
7. **Apply and a join form.** While the running configuration is a join form the apply engine does not hold `metadata.name` and `spec.dataDisk` as never-changed, since the join form names neither.
8. **`spec.api.keyFile`.** `spec.api` takes `keyFile`, an absolute path to a PEM private key read when the Node API starts, as the alternative to `key`. Exactly one is required; both is refused naming `spec.api.keyFile`. A missing or unreadable file is a refusal like a pair that does not load. A joined host's configuration names `/var/lib/naru/hostd/host.key`, which the data disk, mounted at step 5, holds before the Node API starts at step 6.

## Consequences

- A host is given to a cluster by one short `user-data` and no hand-written network.
- Init now carries a DHCPv4 client, with its socket adapter tested only by the QEMU test of garamsh/naru#166.
- Hostd's key and the stored revision live on the data disk, so a replaced data disk starts the join again.
