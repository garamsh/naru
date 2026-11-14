# ADR 0051: the recovery key signs a once-only request for a new root certificate, and the signer revokes every root serial it recorded

> Status: accepted; item 3 and the risk of a restored signer superseded by ADR 0052
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#117 (garamsh/aurora#811 A2 to A5, garamsh/naru#113 decision 8). It closes the open question ADR 0040 left in `signer.md`: a lost root person file.

## Context

- **`restore` needs root.** `POST /v1/restore` takes root's signed authorisation of the bundle (ADR 0040, decision 9), and every call that issues an operator, an orch certificate or a replica is root's. An operator who has lost root's person file, and holds a bundle and the recovery key, could not recover.
- **The recovery key is made for this case.** It is an X25519 key the bundle is sealed to and an Ed25519 key that signs (`lib/identity`); the signer already records its public half in `recovery-public.json`. Its Ed25519 half signs nothing today.
- **A lost root file may be a stolen one.** A new root that leaves the old one admitted fixes the lock-out and keeps the theft.
- **A signed request can be replayed until it expires** (ADR 0040, accepted risks). That is tolerable for a request whose replay repeats an action already done, and not for one whose replay revokes the root a person has just been given.

## Decision

1. **The recovery key's Ed25519 half signs a request for a new root.** `lib/identity` gains `SignRecoveryRequest` and `VerifyRecoveryRequest`: the action, an expiry and a body, signed with the Ed25519 key, with no certificate attached, because the signer holds the recovery key's public half. The action is `reissue-root` and its body is the PKCS #10 certificate request of the new root key and a nonce of 16 random bytes, hexadecimal.
2. **`POST /v1/reissue-root` takes that request and answers with the issued certificate.** The signer verifies the signature against the recorded public half, the action and the expiry, refuses an expiry more than ten minutes ahead, and refuses a nonce shorter than the random one or longer than 128 characters. It issues CN root, OU root, client authentication, one year, under the operator authority, for the request's key.
3. **A request is taken once.** Before anything changes, the signer writes the nonce with the request's expiry to `reissue-nonces.json` in its directory (0600, replaced whole or not at all), and refuses a nonce it finds there with 403. A nonce past its expiry is dropped from the file, because the request fails on its expiry. The file is not in the bundle: a restored or replicated signer starts with none and is guarded by the expiry alone.
4. **Every serial the signer recorded as root's is revoked.** The signer adds all of them, the one `init` issued and every earlier reissue, to the revocation list in one signing with the next sequence number, and writes it before it issues. The new serial is recorded as root's in `issued.json` and so is revoked in turn by the next reissue. The list is the one the signers already share ([ADR 0041](./0041-orch-is-a-cluster-of-members-with-an-embedded-etcd-one-elected-leader-and-an-mtls-api-that-enrolls-and-admits-hosts-and-the-signer-holds-every-key.md)): orch carries it to every signer, which adopts it through `POST /v1/crl`, and from then on that signer refuses the old root.
5. **The reissue is audited.** The signer logs, at warning level, the new serial and key hash, the SHA-256 of the recovery key's public half, the nonce and the root serials it revoked.
6. **`naructl signer reissue-root`** takes `--recovery-key` and `--passphrase-file`, local files, and `--csr`, `--socket` and `--out`. It opens the recovery key on the machine it runs on, signs the request with a new nonce and an expiry of two minutes, and writes the issued certificate. The recovery key is never printed and nothing unencrypted is written. `naructl operator person` then joins the new certificate with the key of the request into a person file.

## Accepted risks

- **The recovery key is root, for this purpose.** Whoever holds it and its passphrase can make a root, and so can open the bundle it already opens. Reissuing adds a way to act, not a way to read what the bundle did not already give.
- **A root serial the signer has no record of stays admitted.** A root issued before the signer kept `issued.json` is not on the list the reissue writes; the operator revokes it by serial.
- **A replica that has not taken the list admits the old root until it does.** Revocation reaches a signer when orch's sync pushes the list; before then that signer's own list is the one it checks.
- **A restored or replicated signer has no record of the nonces.** A request captured before the restore can be replayed there until its expiry, at most ten minutes after it was made.
- **A reissue that fails after it wrote the list leaves the old root revoked and no new one.** The signer returns the error and the holder of the recovery key signs a new request.

## Consequences

- A lost root person file is recovered with the recovery key alone, on a running signer, with no bundle and no restore.
- `lib/identity` gains the recovery-signed request and its body type; `host/internal/signer` gains `ReissueRoot` and the nonce file; `naructl signer` gains `reissue-root`.
- The recovery key has a second use, which makes its passphrase and its custody weigh what root's do.

## Signs the decision was wrong

- A root is reissued without the recovery key's holder asking: the signature or the replay guard is not the defence this states.
- A captured request is accepted after a restore and used: the nonce record must travel with the bundle.
- Operators need to recover root without the recovery key: the lost-root case wants a second path, such as a quorum of operators.
