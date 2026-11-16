# Rotate a host's key pair

Replace one host's key and certificate through the signer, with the old serial put on the revocation list. It ends a leaked or retired pair; it does not rotate an authority's key. The design is [ADR 0077](../architecture/adr/0077-the-signer-rotates-a-hosts-pair-with-the-old-serial-revoked-registers-offline-pairs-by-serial-and-hostd-refuses-a-revoked-key.md).

`<admin>` is root's or an operator's encrypted person file, and each command takes `--socket` or `--config` as the other signer commands do ([first-hosts.md](./first-hosts.md)). `TestFleet_RotatingAHostRevokesItsOldPairAndTheHostRefusesARevokedKey` (`tooling/qemu/tests/rotate_fleet_test.go`) runs steps 2 and 3.

## 1. A pair `gen node` wrote: register its serial

A host whose certificate the signer issued is already known to it. A `gen node` pair is issued offline from the host authority, so the signer holds no record of it: register the certificate, from the user-data's `/run/naru-hostd/tls.crt` saved as `<tls.crt>`:

```
naructl signer register-host --admin <admin> --passphrase-file <passphrase file> --host-id <host ID> --cert <tls.crt>
```

It prints `naructl: registered serial <serial> for <host ID>; rotate-host revokes it`. The command refuses a certificate whose CN is not `--host-id`. A host the signer has no serial for is refused by the next step with a message that names this one.

## 2. Rotate

```
naructl signer rotate-host --admin <admin> --passphrase-file <passphrase file> --host-id <host ID> --key-out <dir>/<host ID>.key --cert-out <dir>/<host ID>.crt
```

It prints `naructl: rotated <host ID>: the old serial is revoked; wrote the new key <key> (0600) and certificate <cert>, serial <serial>, valid until <time>`. The signer revoked every serial it holds for the host; the list reaches the peers within its TTL (30 seconds) and the other signers at the next sync. It writes nothing when the signer refuses.

## 3. Deliver the pair and see the old one refused

Put the new certificate and key where the host reads them (`spec.host.certificate` and the key file it names, or the files of `spec.files` that write `/run/naru-hostd/tls.crt` and `tls.key`) and apply the configuration. Until then the host still holds the old pair: `naru-hostd` checks its own certificates every minute, and on a revoked one it logs `refusing to serve with a revoked key` with the file, the subject and the serial, and exits 1. A host that boots from a seed or a stored configuration that carries a revoked pair is refused the same way at start. Destroy the old key.

Check it with `naructl host list`: the host reports to orch again once its new certificate serves.
