# Rotate a host's key pair

Replace the pair one host reports to orch with through the signer and the host API, and revoke the old serial once the host holds the new pair. It ends a leaked or retired pair; it does not rotate an authority's key. The design is [ADR 0077](../architecture/adr/0077-the-signer-rotates-a-hosts-pair-with-the-old-serial-revoked-registers-offline-pairs-by-serial-and-hostd-refuses-a-revoked-key.md).

`<admin>` is root's or an operator's encrypted person file, and each command takes `--socket` or `--config` as the other signer commands do ([first-hosts.md](./first-hosts.md)). `<host client>` is a provisioner's file from `naructl gen host-client`; the host must be one that client already reaches. `TestFleet_RotatingARunningHostDeliversTheNewPairBeforeTheOldIsRevoked` and `TestFleet_RotatingAHostRevokesAnOfflinePairRegisteredBySerialAndTheHostRefusesItAfterARestart` (`tooling/qemu/tests/rotate_fleet_test.go`) run steps 1 to 3.

## 1. A pair `gen node` wrote: register its serial

A host whose certificate the signer issued is already known to it. A `gen node` pair is issued offline from the host authority, so the signer holds no record of it: register the certificate, from the user-data's `/run/naru-hostd/tls.crt` saved as `<tls.crt>`:

```
naructl signer register-host --admin <admin> --passphrase-file <passphrase file> --host-id <host ID> --cert <tls.crt>
```

It prints `naructl: registered serial <serial> for <host ID>; rotate-host revokes it`. The command refuses a certificate whose CN is not `--host-id`. A host the signer has no serial for is refused by the next step with a message that names this one.

Register the serving pair only when you mean to end it. Rotation puts a new pair beside `--host-key` and replaces the pair the host reports to orch with; it does not replace the serving pair of `/run/naru-hostd/tls.crt`, which is configuration. The rotation revokes a registered serving pair, and `naru-hostd` then refuses to serve with it (step 3). Replace it by configuration, as in that step.

## 2. Rotate

```
naructl signer rotate-host --admin <admin> --passphrase-file <passphrase file> --host-id <host ID> --host-client <host client>
```

`--host` and `--host-server-name` override the client file's endpoint and the name the host's certificate is checked against, which is `h-<id>` for a host that joined from the install seed ([first-hosts.md](./first-hosts.md)). The command does four things in this order:

1. The signer issues a new certificate for a key made on this workstation and revokes nothing.
2. The host takes the pair through `PUT /v1/identity`. It reports to orch with it first, and writes it only when orch accepts.
3. naructl checks that the serial the host reports with is the new one.
4. The signer revokes every other serial it holds for the host. The list reaches the peers within its TTL (30 seconds) and the other signers at the next sync.

It prints `naructl: rotated <host ID>: it reports to orch with the new pair, serial <serial>, valid until <time>, and its old serials are revoked`. The key is written nowhere but the host.

If it fails before step 4, the old pair is valid and in use, and the message says so; the new serial is revoked by the next rotation. If it fails at step 4, the host already reports with the new pair and the message says to run the command again.

## 3. Check it, and replace the serving pair by configuration

Check it with `naructl host list`: the host keeps reporting to orch, its `LastSeen` advancing, and a client that presents the old pair is refused once the list has reached orch.

`naru-hostd` checks the certificates of `--cert` and beside `--host-key` every minute. On a revoked one it logs `refusing to serve with a revoked key` with the file, the subject and the serial, and exits 1. A host that boots from a seed or a stored configuration that carries a revoked pair is refused the same way at start. To replace a serving pair, put the new certificate and key where the host reads them (`spec.host.certificate` and the key file it names, or the files of `spec.files` that write `/run/naru-hostd/tls.crt` and `tls.key`) and apply the configuration before the old serial is retired, or accept that the host stops until it is applied. Destroy the old key.
