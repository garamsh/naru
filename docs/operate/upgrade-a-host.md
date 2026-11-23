# Upgrade a host

Move a running host or Node to the next release in place with the release's upgrade bundle, read whether it was kept, and what a refused upgrade leaves. It does not recreate a Node from a new boot disk, which a release's notes require when its kernel does not fit the trial slot of the installed image. The designs are [ADR 0025](../architecture/adr/0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md) and [init.md](../architecture/init.md).

`<client file>` is the file `naructl gen api-client --role admin` writes, passed as `--config` to the Node API of the Node being upgraded ([first-hosts.md](./first-hosts.md)); `--endpoint` names the Node when the file lists more than one. `<release dir>` holds the assets `gh release download <tag>` writes. `TestRelease_AHostInstalledFromOneReleaseUpgradesInPlaceToTheNextAndKeepsItsData` (`tooling/qemu/tests/release_upgrade_test.go`, run by `make test-release-upgrade FROM=<dir> TO=<dir>`) runs steps 2 to 4, and the `TestNode_Upgrade…` tests of `tooling/qemu/tests/upgrade_test.go` run step 5.

## 1. Check the release and the Node

- Verify the release first ([verify-a-release.md](./verify-a-release.md)).
- Read the release's notes for the paragraph that tells Nodes of an earlier image to be recreated. When it is there, the bundle's kernel does not fit the Node's trial slot: recreate the Node from `naru-host-<tag>.qcow2` and stop here.
- Read the Node's slot and last upgrade:

```
naructl upgrade --status --config <client file>
```

It prints the `version`, the boot ID, the `slot` (`trial` from a trial boot until it is promoted, otherwise `default`) and an `upgrade` line with how the last upgrade ended, and a `reason` line when it did not end in a promotion.

- A host that runs groups: `naructl host drain <host ID>` moves its members off before the upgrade, and `naructl host undrain <host ID>` places on it again after step 4. Both talk to orch, so they take orch's flags (`--orch-url`, `--orch-ca`, `--identity`) in place of `--config`.

## 2. Send the bundle

```
naructl upgrade --bundle <release dir>/naru-host-upgrade-<tag>.tar --sums <release dir>/SHA256SUMS --config <client file>
```

`naructl` checks the bundle against `SHA256SUMS` before it dials the Node, and sends nothing when they differ. The Node writes the kernel to the trial slot, answers `the Node accepted the upgrade:` with the actions it took, and reboots.

## 3. Wait for the trial boot

The Node boots the trial slot once. It promotes the slot when every service it started has run 30 s without a restart, and rolls back when that has not happened 180 s after init started or when the boot sequence refuses.

## 4. Read the result

```
naructl upgrade --status --config <client file>
```

- Kept: the `version` is the release's, the `slot` is `default`, and the `upgrade` line reads `<old> -> <new>, promoted`. The data disk and the stored configuration are as they were.
- Rolled back: the `version` is the old one and the `upgrade` line says `rolled back`, with the `reason`; for a refused boot the reason is also in `TRIAL.WHY` beside the trial slot on the ESP until the default boot removes it ([ADR 0091](../architecture/adr/0091-a-trial-boot-that-gives-up-leaves-its-reason-in-a-file-beside-the-trial-slot-on-the-esp.md)). Read the service logs with `naructl logs <service> --config <client file>`, fix the cause, and send the bundle again.

## 5. What a refusal looks like

- A bundle whose digest, manifest or kernel size is wrong is refused with 400 and one over 4 GiB with 413; neither touches the boot slot in use.
- A Node whose configuration lists a system extension with no stored image built for the bundle's version is refused with 422; an entry without a digest is resolved to the image the bundle carries ([ADR 0084](../architecture/adr/0084-an-upgrade-bundle-carries-the-extensions-the-hosts-configuration-names-and-an-entry-without-a-digest-is-resolved-by-init-to-the-image-built-for-its-version.md)), and a refused upgrade that carried an extension removes the new one and keeps the old.
- An upgrade that cannot start now is refused with 409: read `naructl upgrade --status --config <client file>` and try again once the Node is settled.
