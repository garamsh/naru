# Recover a lost control member or host

Bring back one orch member whose etcd data is lost or full, and one host that is lost, refused at boot or has to be reset, while the rest of the fleet runs. It does not cover the loss of every control host ([recover-the-control-plane.md](./recover-the-control-plane.md)), and a Node is replaced and not reset ([ADR 0082](../architecture/adr/0082-a-host-resets-its-store-its-data-disk-or-both-through-post-v1-reset-at-its-next-boot-and-a-refused-host-serves-maintenance-mode-to-operators-under-orchs-host-ca.md)). The designs are [ADR 0088](../architecture/adr/0088-orch-compacts-defragments-and-bounds-its-etcd-by-a-share-of-the-data-disk-drops-events-by-age-removes-a-member-that-stays-out-for-it-to-rejoin-empty-and-init-grows-the-data-file-system-to-its-disk.md) for the member and [ADR 0082](../architecture/adr/0082-a-host-resets-its-store-its-data-disk-or-both-through-post-v1-reset-at-its-next-boot-and-a-refused-host-serves-maintenance-mode-to-operators-under-orchs-host-ca.md) for the host, with [orch.md](../architecture/orch.md) and [host.md](../architecture/host.md).

The orch commands take `--orch-url`, `--orch-ca`, `--identity` and `--passphrase-file`, and the host commands the client file of `naructl host`, as in [first-hosts.md](./first-hosts.md). `TestFleet_TheLeaderRemovesAMemberThatStaysOutOnlyWhileTheOthersAnswerAndItRejoinsEmptyWhenItStarts` (`tooling/qemu/tests/housekeeping_fleet_test.go`) runs part 1, and `TestFleet_AWipeOfTheDataDiskMakesTheHostFormatItAgainAtItsNextBoot` and `TestFleet_ARefusedHostIsReachableInMaintenanceByACertificateUnderOrchsAuthorityAndRefusedOnceItIsRevoked` (`tooling/qemu/tests/reset_fleet_test.go`) run parts 2 and 3.

## 1. A control member that lost its data, or whose disk is full

No command is needed while the other members keep their quorum: the leader removes a member that has been unreachable for 30 minutes (`spec.orch.departAfter`), when the cluster has three members or more and every other member answers, and the member rejoins empty when it starts.

1. Check the other members answer: `naructl group list` and `naructl host list` on them still work.
2. Make the member start with room. A member whose data disk filled up has no space for the log it replays, so give it room first: enlarge the data disk at the hypervisor, and reboot the Node, since init grows the data file system to its disk at boot (`data disk file system grown` on the console).
3. Or give it a blank data disk and the Node's user-data, as in [recover-the-control-plane.md](./recover-the-control-plane.md) step 2, with `spec.orch.join` set to a running member's client URL and the state `existing`, and no `initialCluster`.
4. Start the Node. At start the member asks the cluster whether it still lists a member of its name; one that is no longer listed discards its member directory (`discarded the member directory of a member the cluster removed`) and adds itself through the peers, then syncs from them.
5. Read it back: `naructl host list` and `naructl group list` on the rejoined member match the others, and its console shows no `no space left on device`.

A member that is down for 30 minutes of maintenance is removed too and rejoins empty by the same path. The quota of one eighth of the data disk, within 128 MiB and 2 GiB, and a disk under 1 GiB refused at start, are in ADR 0088: a control host's data disk is 8 GiB in the release's harness.

## 2. A host that is lost

1. Move what it runs: a stateless group keeps its count on the hosts that answer, so its member is made on another host that fits with no command (`TestFleet_AStatelessMemberOfAHostThatStopsAnsweringIsMadeOnTheOtherHostWithNoDrain`, `tooling/qemu/tests/lost_host_fleet_test.go`), and a stateful member waits until its fence has lapsed before it is made elsewhere (`TestFleet_GroupsKeepTheCountRollDrainReplaceOnALabelChangeAndFenceAStatefulMember`, `tooling/qemu/tests/orch_fleet_test.go`). `naructl host drain <host ID>` moves the members ahead of time while the host still answers.
2. Forget it: `naructl host remove <host ID>`. It frees the host's Machine prefix, and its certificate stays valid.
3. Revoke the certificate: `naructl operator revoke --admin <admin> --passphrase-file <passphrase file> --serial <serial>` (the serial from the host's records, or from `naructl signer register-host` when the pair was issued offline, [rotate-a-host.md](./rotate-a-host.md)).
4. A replacement joins as a new host: [first-hosts.md](./first-hosts.md) steps 8 and 9.

## 3. A host that runs but must be reset

`naructl reset --scope <scope> --confirm <host ID> --config <host client>` reboots the host, and init carries the reset out after it mounts the data disk. `<host ID>` is `hostId` of `naructl host`; a different one is refused with nothing done.

- `store`: removes the stored configuration, so the host runs from its seed again at revision 0.
- `data`: removes everything on the data disk but the stored configuration.
- `all`: blanks the data disk, and the host formats it again at its next boot.

For `data` and `all` the call is refused with 409 `revoked-pair` while the pair the configuration names is revoked: rotate the pair first ([rotate-a-host.md](./rotate-a-host.md)).

## 4. A host that refuses to boot

A host whose boot sequence was refused serves maintenance mode: only `GET /v1/maintenance` and `POST /v1/reset`, to client certificates under orch's host authority, so an operator certificate reaches it and a revoked one does not.

1. Read the reason from the host's console, which logs why the boot was refused, or from `GET /v1/maintenance` with an operator's certificate.
2. Reset it with the scope the reason points to (part 3), usually `store`, and wait for the next boot.

Maintenance mode fails closed. A host that never enrolled, and one whose refusal came before the network with no orch fingerprint on its seed, has none: such a host is reached on its console and re-installed as in first-hosts.md.
