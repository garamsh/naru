# ADR 0077: the signer rotates a host's pair with the old serial revoked, registers offline pairs by serial, and hostd refuses a revoked key

> Status: accepted
> Date: 2026-11-16

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#237, part of goal garamsh/naru#235. It reads [ADR 0040](./0040-naru-signer-alone-holds-the-authority-keys-in-a-private-directory-and-issues-certificates-over-a-unix-socket-against-signed-requests-and-a-recovery-key-opens-the-bundle-that-restores-them.md) and [ADR 0062](./0062-every-naru-tls-peer-is-checked-against-the-signed-revocation-list-by-one-verifier-in-lib-identity-and-fails-closed-when-the-list-is-older-than-its-max-age.md) as they stand. Rotating an authority's own key is out of scope.

## Context

- **A host's key could be revoked but not replaced.** `operator revoke` puts a serial on the list, and a joined host's certificate comes from the signer only for an admission statement, which names the host's own key hash. A host with a leaked key had no operation that ended the old certificate and gave it a new one.
- **A pair `gen node` writes is invisible to the signer.** `gen node` issues the host's serving pair offline from the host authority `gen host-ca` wrote, which is not the signer's host authority. The signer never saw its serial, so the operator had no record to revoke it by.
- **Nothing on the host looked at its own key.** `naru-hostd` checks the clients' certificates against the revocation list (ADR 0062) and never its own serving certificate or the one stored beside its key, so a seed or a stored configuration that carried a revoked pair was served with.
- **The user decided that the signer issues and revokes.** Orch, hostd and naructl add no authority.

## Decision

1. **The signer keeps a registry of host certificate serials by host ID.** `hosts.json` in its directory (0600, optional) maps a host ID to the serials of its certificates. `IssueHost` records the serial it issues, and the registry is carried by the bundle, so a restored signer holds it. It is written in one commit with `issued.json` and the revocation list (`commitFiles`).
2. **`POST /v1/rotate-host`, root or an operator, rotates a host.** The signed request names the host ID and the certificate request of the new key. The signer issues a host certificate for it (CN and DNS name the host ID, one year), puts every serial the registry holds for the host on the revocation list at the next sequence, and replaces the host's entry with the new serial, in one commit. A host with no serial in the registry is refused with 409 naming `register-host`. Operators could already issue a host certificate through an admission statement and revoke a serial, so the call adds no authority.
3. **`POST /v1/register-host`, root or an operator, registers an offline pair by serial.** The request names the host ID and the serial of a certificate another authority issued. The signer cannot verify that certificate: it takes the serial from the operator who signs, refuses a serial recorded as root's or any other role's, and does nothing for a serial already registered. A later rotation revokes it like any other. `naructl signer register-host --cert` reads the serial from the certificate, and refuses a certificate whose CN is not `--host-id`.
4. **`naructl signer rotate-host` makes the new key on the workstation** and writes it 0600, with the certificate, only when the signer has issued it. Delivering the pair to the host is a configuration change the operator makes, `docs/operate/rotate-a-host.md`.
5. **`naru-hostd` refuses a revoked key.** At start, and every minute while it runs, it checks every certificate of `--cert` and of the certificate beside `--host-key` against the revocation list. When one is on it, hostd logs the file, the subject and the serial and exits 1, and never serves. That covers a seed (`spec.files`, `spec.host.certificate`) and a stored configuration, which both reach hostd as those files. A list that cannot be had yet is logged and the check retried, and the host's seed list (`--revocation-seed`) is what lets the first check run with no signer reachable.
6. **A `gen node` pair is registered by the operator after `gen node`, not by it.** `gen node` writes the certificate into the user-data; the operator runs `signer register-host` on it. Doing it from `gen node` needs `naructl`'s `gen_node.go` and a signer reachable while the user-data is made offline, which the pair's offline issue does not assume; it is left out of this decision.

## Consequences

- **A leaked host key is ended in two commands.** `rotate-host` and the configuration change that delivers the pair; the old pair is refused by every peer within the list's TTL, and by the host itself within a minute.
- **A rotated host the signer never registered cannot be rotated.** The 409 says to register first. A joined host, whose certificate the signer issued, is already registered if the signer's directory was written by this version; one admitted by an earlier version is not, and is registered like an offline pair.
- **A replica holds the registry only as the bundle it was restored from carried it.** A host admitted through another replica since is unknown to this one, and `rotate-host` there is refused with 409 until the operator uses the signer that admitted it or registers the serial. The revocation list itself is shared as before.
- **The Node of a revoked host still boots.** Only `naru-hostd` refuses: init's boot and the Node API are not stopped by this ADR.
- **hostd checks files, not what a peer sees.** A revoked key an attacker holds is refused by the peers as before; the host's own refusal is for the honest host that was handed a revoked pair.

## Signs the decision was wrong

- Operators register serials wrongly often enough that a rotation revokes nothing, or the wrong host: the signer should read the certificate itself, or `gen node` should register.
- Rotation is wanted where the signer is not reachable, which would need the key made on the host and a certificate request from it.
- A host that exits on a revoked key and is restarted by init in a loop hides the cause: the exit should be reported to orch.
