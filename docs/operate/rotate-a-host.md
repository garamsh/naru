# Rotate a host's key pair

Replace the pair one host reports to orch with through the signer and the host API, and revoke the old serial once the host holds the new pair. It ends a leaked or retired pair; it does not rotate an authority's key. The design is [ADR 0077](../architecture/adr/0077-the-signer-rotates-a-hosts-pair-with-the-old-serial-revoked-registers-offline-pairs-by-serial-and-hostd-refuses-a-revoked-key.md).

`<admin>` is root's or an operator's encrypted person file, and each command takes `--socket` or `--config` as the other signer commands do ([first-hosts.md](./first-hosts.md)). `<host client>` is a provisioner's file from `naructl gen host-client`; the host must be one that client already reaches. `TestFleet_RotatingARunningHostDeliversTheNewPairBeforeTheOldIsRevoked` and `TestFleet_RotatingAHostRevokesAnOfflinePairRegisteredBySerialAndTheHostRefusesItAfterARestart` (`tooling/qemu/tests/rotate_fleet_test.go`) run steps 1 to 3.

## 0. Trust both host authorities

The pair a rotation delivers is issued by the signer's host authority, which is not the offline authority `naructl gen host-ca` wrote. A deployment that rotates through the signer gives its hosts' clients both authorities, or they refuse the new pair:

1. Read the signer's host authority with `naructl signer ca` (`GET /v1/ca`) and keep it as `<signer host ca>`.
2. Orch: put it beside the offline authority in one PEM file and give that file to `gen node --orch-host-ca` (`spec.orch.hostCA`). Orch trusts every certificate of the file for the hosts' pairs. `gen node` accepts a file of several certificates.
3. The operator: append it to the `ca` of the `<host client>` file the rotation uses and of every other client of the host API.

Skip this only for a host whose serving certificate the signer's host authority already issued.

## 1. A pair `gen node` wrote: register its serial

A host whose certificate the signer issued is already known to it. A `gen node` pair is issued offline from the host authority, so the signer holds no record of it: register the certificate, from the user-data's `/run/naru-hostd/tls.crt` saved as `<tls.crt>`:

```
naructl signer register-host --admin <admin> --passphrase-file <passphrase file> --host-id <host ID> --cert <tls.crt>
```

It prints `naructl: registered serial <serial> for <host ID>; rotate-host revokes it`. The command refuses a certificate whose CN is not `--host-id`. A host the signer has no serial for is refused by the next step with a message that names this one.

The rotation replaces a registered serving pair too: the host serves the new pair at once, without a restart, and keeps it after one, though its seed still carries the old pair, because the host stores the pair beside `--host-key` (`serving.pem`) and serves that first.

## 2. Rotate

```
naructl signer rotate-host --admin <admin> --passphrase-file <passphrase file> --host-id <host ID> --host-client <host client>
```

`--host` and `--host-server-name` override the client file's endpoint and the name the host's certificate is checked against, which is `h-<id>` for a host that joined from the install seed ([first-hosts.md](./first-hosts.md)). The command does four things in this order:

1. The signer issues a new certificate for a key made on this workstation and revokes nothing.
2. The host takes the pair through `PUT /v1/identity`. It reports to orch with it first, and stores and serves it only when orch accepts.
3. naructl checks that the serial the host reports with is the new one, and waits up to 30 seconds until the host API presents the new certificate on a new connection.
4. The signer revokes every other serial it holds for the host. The list reaches the peers within its TTL (30 seconds) and the other signers at the next sync.

It prints `naructl: rotated <host ID>: it reports to orch with the new pair, serial <serial>, valid until <time>, and its old serials are revoked`. The key is written nowhere but the host.

If it fails before step 4, the old pair is valid and in use, and the message says so; the new serial is revoked by the next rotation. If it fails at step 4, the host already reports with the new pair and the message says to run the command again.

## 3. Check it, and what a revoked key does

Check it with `naructl host list`: the host keeps reporting to orch, its `LastSeen` advancing, and a client that presents the old pair is refused once the list has reached orch.

`naru-hostd` chooses its serving pair at start: the stored `serving.pem` when it is not revoked, else the seed pair (`/run/naru-hostd/tls.crt`) when that is not revoked, else it logs `refusing to serve with a revoked key` with the file, the subject and the serial, and exits 1. It checks the pair it serves and the one beside `--host-key` every minute and exits the same way. Only `naru-hostd` stops: init and the Node API keep running.

If the host's data disk is wiped, the stored pair is lost and the seed pair applies again. When that is revoked the host does not start: issue a new seed for it with `gen node` and apply it. Destroy the old key.
