# Recover the control plane

Bring the signer and orch back on blank control hosts from the signer's bundle and orch's sealed snapshot, after every control host lost its disk. It does not replace one member of a running cluster (a member that stays out rejoins by itself, [ADR 0088](../architecture/adr/0088-orch-compacts-defragments-and-bounds-its-etcd-by-a-share-of-the-data-disk-drops-events-by-age-removes-a-member-that-stays-out-for-it-to-rejoin-empty-and-init-grows-the-data-file-system-to-its-disk.md)). The designs are [signer.md](../architecture/signer.md) (bundle and restore) and [orch.md](../architecture/orch.md) (snapshots and restore, [ADR 0053](../architecture/adr/0053-a-member-started-with-restore-serves-only-a-snapshot-upload-and-the-operator-sends-it-the-plain-snapshot-and-the-other-members-join-it-as-new-members.md)).

`<admin>` is root's encrypted person file, `<passphrase file>` holds its passphrase, and `<recovery key>` is the file `naructl orch init` wrote ([first-hosts.md](./first-hosts.md), step 2) with `<recovery passphrase file>` for it. The orch commands take `--orch-url` and `--orch-ca` and the signer commands `--socket` or `--config`, as in first-hosts.md. `TestFleet_RestoresTheControlHostsFromTheSignersBundleAndOrchsSealedSnapshot` (`tooling/qemu/tests/orch_fleet_test.go`) runs steps 2 to 5 on three QEMU control hosts that all lost their disks.

## 1. Keep the two things the recovery needs

Do this while the control plane is healthy, and again after any change of the signer's authorities or operators. The recovery key and its passphrase are kept apart from the control hosts.

```
naructl signer bundle --admin <admin> --passphrase-file <passphrase file> --config <signer config> --out <bundle>
naructl orch backup --orch-url <orch URL> --orch-ca <orch CA> --identity <admin> --passphrase-file <passphrase file> --out <sealed snapshot>
```

The bundle holds every authority key sealed to the recovery key. The snapshot is sealed to the recovery key too; orch also takes one every 6 hours on each member into `/var/lib/naru/orch/snapshots`, and `orch backup` fetches a fresh one. Both files are copied off the control hosts.

## 2. Boot the control hosts blank

Each control host keeps its user-data and gets a blank data disk. Host 1 boots without `spec.orch` and without `spec.signer.bootstrap`: it is restored, not bootstrapped. Hosts 2 and 3 boot with `spec.signer.replica` and no `spec.orch`. Host 1's user-data carries the recovery key and its passphrase as two files readable only by the signer's user (mode `0600`, owner `6001`), which the next step names by path.

## 3. Restore the signer on host 1

```
naructl signer restore --admin <admin> --passphrase-file <passphrase file> --bundle <bundle> \
  --recovery-key <path on the host> --recovery-passphrase-file <path on the host> --config <signer config>
```

The paths are the two files of step 2, read on the host. The certificates issued before the loss verify against the restored authorities, so the hosts and operators keep their pairs.

## 4. Restore orch on host 1

1. Issue host 1's orch certificate from the restored signer and place it as in first-hosts.md step 4, and boot host 1 with `spec.orch` set as before plus `restore: true` (`naructl gen node --orch-restore`), with itself alone as the cluster. The member starts no etcd and serves only `PUT /v1/restore`.
2. Open the sealed snapshot on the workstation and send it:

```
naructl orch restore --snapshot <sealed snapshot> --recovery-key <recovery key> --passphrase-file <recovery passphrase file> \
  --push <orch URL of host 1> --orch-ca <orch CA> --admin <admin> --admin-passphrase-file <passphrase file>
```

The member refuses a snapshot whose hash or etcd's own check fails, restores into its own name and starts. Without `--push`, `--out <file>` writes the plain snapshot to the workstation's disk (mode 0600) instead; it holds every group and host record, so delete it after use.

## 5. Join hosts 2 and 3

For each: authorise its signer replica as in first-hosts.md step 5 (`naructl signer authorise-replica`), issue its orch certificate, and boot it with `spec.orch.join` set to host 1's client URL (`naructl gen node --orch-join <URL>`) and the initial cluster state `existing`, and no `initialCluster`. Each member adds itself to the cluster and syncs from host 1.

## 6. Check it

- `naructl group list` and `naructl host list` on every control host show the groups and hosts of the snapshot.
- Each admitted host reconnects with the certificate it had: its `lastSeen` in `naructl host list` moves after the restore.
- What the snapshot is missing is what happened after it was taken: a group applied since then is gone, and is applied again.
