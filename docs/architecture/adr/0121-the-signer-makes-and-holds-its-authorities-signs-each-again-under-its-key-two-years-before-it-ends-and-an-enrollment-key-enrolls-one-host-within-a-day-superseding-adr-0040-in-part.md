# ADR 0121: the signer makes and holds its authorities, signs each again under its key two years before it ends, and an enrollment key enrolls one host within a day, superseding ADR 0040 in part

> Status: accepted
> Date: 2026-11-28

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#412. It supersedes in part [ADR 0040](./0040-naru-signer-alone-holds-the-authority-keys-in-a-private-directory-and-issues-certificates-over-a-unix-socket-against-signed-requests-and-a-recovery-key-opens-the-bundle-that-restores-them.md):
- item 3's authorities "valid ten years", which said nothing of what happens at the end;
- item 8's custody, which this record states as the target of #412: no person makes, places or holds an authority key;
- the accepted risk "A host certificate lasts one year and has no renewal".

It also replaces the enrollment key's life in `orch.md` §Enrollment, "valid at most 365 days". The rest of ADR 0040 stands, and its body is not edited.

## Context

- **sauce's direction (2026-11-28).** naru's authority keys live inside the signer, as kubeadm keeps them, with no KMS or OpenBao (`garamsh/discussions` discussion 6, comment 19964427). Every host, orch and caller certificate is renewed by its holder before it expires. The recovery key is an emergency key, kept offline on USB (comment 19858505).
- **What ADR 0040 left open.**
  - Each authority's certificate is self-signed for ten years, and nothing renews it.
  - Go's `crypto/x509` checks a root's own validity, so on the day a certificate ends every pool that holds only it stops trusting every certificate under it.
- **Every trust anchor is a file.** Each is written at install and is not read again. They are:
  - orch's `--ca` and `--host-ca`;
  - hostd's `--client-ca` and `--orch-ca-sha256`;
  - the Node API's CA;
  - naructl's `--orch-ca`.
- **An enrollment key was a bearer secret for up to a year.** Any host that held it could enroll, as often as it was presented, until it expired or was revoked.
- **The PM's decisions on #412 (2026-11-28).**
  1. The bundle, sealed to the USB recovery key, stays the disaster path. "No plain key leaves" means no authority key leaves the signer unsealed, and replication between signers is unchanged.
  2. The work lands as six pull requests, in order, each from main once the previous one has merged:
     - (a) this record, the authorities' rollover and the enrollment keys;
     - (b) the host's renewal;
     - (c) orch's renewal;
     - (d) people's renewal;
     - (e) the callers' `crypto.Signer` check, and the revocation key behind `crypto.Signer`;
     - (f) the host API's and the Node API's authorities moved into the signer.
  3. Each authority is signed again under the same key when under 2 of its 10 years remain, and peers fetch the new certificate from `GET /v1/ca`. Each consumer's trust refresh lands with its renewal: hostd's in (b), orch's in (c), people's files in (d). The 2-year window is the bound by which every pool refreshes.
  4. A check warns, as an event and a log, when a pool still holds an authority certificate within a year of its end.
  5. Key rotation, a new key with a cross-signed overlap, is not built (garamsh/naru#451).
  6. ACME for the proxy stays with garamsh/naru#395.
  7. An enrollment key is bound to the first host and public key that present it, in the transaction that creates the host's pending record. That host's repeats go on, and any other host is refused as an unknown key is. The key lives at most 24 hours, and once its host is admitted it is spent for good.

## Decision

1. **The signer makes and holds its authorities.**
   - `naru-signer init` generates the host, operator and orch authorities (ECDSA P-256) and the revocation key (Ed25519) in its own directory, as ADR 0040 items 2 and 3 decide.
   - No person makes, places or rotates one of these keys, and no `naructl` command takes one. The host API's and the Node API's authorities that `naructl gen api-ca` and `naructl gen host-ca` make (`docs/operate/first-hosts.md` step 1) are not the signer's yet: they move into the signer as part (f) of garamsh/naru#412, after (e) (the PM, 2026-11-28).
   - An authority key leaves the directory only sealed: in the bundle, to the recovery key (ADR 0040 item 8), or in a replica, to a joining signer's ephemeral key (item 10).
   - The recovery key is made by its holder outside the signer, is kept offline on USB, and never enters a signer's directory.
2. **Every certificate an authority signs is signed through `crypto.Signer`.** The authority's key in the directory is the implementation now. A KMS or HSM key is a later implementation behind the same interface, which no caller sees. The revocation key signs the list through `lib/identity`'s `SignCRL` with its Ed25519 key, and moves behind `crypto.Signer` in (e) (the PM, 2026-11-28).
3. **An authority is signed again under its key two years before it ends.**
   - The signer checks at start and every 24 hours. Each authority with less than `identity.AuthorityRenewYears` (2) of its `identity.AuthorityYears` (10) left is signed again: the same key and subject, a new serial, valid ten years from then.
   - The new certificates are written in one commit, and from then on are what `GET /v1/ca`, the bundle and a replica carry.
   - A certificate the old one signed verifies against the new, and one the new signed verifies against the old within the old one's life, because the key is the same.
   - Each signer of a cluster signs again on its own check, so two members may serve different certificates of one authority, each valid. A pool may hold both.
4. **Every pool refreshes within the two years.**
   - A holder of a pool takes the authorities' certificates from `GET /v1/ca` and adds them to its pool before the old ones end: hostd in (b), orch in (c), the people's files in (d).
   - A pin of an authority by its certificate's hash, such as hostd's `--orch-ca-sha256`, names one certificate and does not survive a re-signing. (b) moves it to the authority's key.
5. **A pool within a year of its end is warned of.**
   - A pool's end is, of each authority it holds (one subject and one key), the latest end among that authority's certificates, and the earliest of those ends, with the subject of the authority that sets it (`identity.TrustExpiry`).
   - hostd reports its `--client-ca` pool's end in `GET /v1/host` as `trustExpiresAt`, and the authority as `trustExpiresAuthority`, read from the file on each request. That pool may hold authorities the signer does not, such as the one `naructl gen host-ca` made, which the signer re-signs only once (f) has moved them into it; the warning names the authority, so the operator sees which one ends.
   - Every orch member checks its own `--ca` and `--host-ca` every hour. The leader checks every admitted host's report on the same interval.
   - A pool that ends within `identity.AuthorityWarnYears` (1), or has ended, is logged and appended as `authority.expiring`. The event's subject is `orch/<member>` or the host's ID, and its detail names the pool, the file where orch read it, the authority and the end. It is appended once per subject, pool, authority and end in a process's life, and a failed append is tried at the next check.
6. **Key rotation is not built.**
   - A new key for an authority, with an overlap in which each key cross-signs the other, is garamsh/naru#451.
   - Re-signing under the same key covers the end of a certificate, not the compromise of a key. A compromised authority key is the restore of ADR 0040 item 9 with new authorities.
7. **Holders renew their own certificates.**
   - hostd renews its host certificate under its key before it ends, and the old serial is revoked once the new one is in use: (b).
   - Orch renews its own: (c).
   - Root and each operator renew theirs from their own device: (d).
   - A caller renews as ADR 0116 decides, and (e) checks that the renewal goes through `crypto.Signer`.
8. **An enrollment key enrolls one host, within a day.**
   - `POST /v1/enrollment-keys` refuses an expiry more than 24 hours and five minutes from orch's now with 400 (`inventory.MaxKeyLife`, `inventory.KeyClockSkew`), and cuts one within the five minutes back to 24 hours. The five minutes are for a client whose clock is ahead of orch's, such as naructl's default `--ttl 24h`, and no key lives longer than 24 hours by orch's clock. A key enrolls only until its expiry or 24 hours and five minutes after it was made, whichever is first, which bounds a key made before this record with a life of up to 365 days.
   - The first `POST /v1/enroll` that presents a key binds it to its host ID and public key, in the transaction that creates the host's pending record.
   - The same host and key may present it again, as the host's agent does when it retries.
   - Any other host ID or key is refused with 403, with the body an unknown, expired or revoked key gets. Every refusal of a key, spent or lost to another enrollment's race included, answers that one body (`enrollment key refused`); orch logs the reason, and a refusal of a stored key is the event `enrollment.refused`, whose subject is the key's ID and whose detail names the host and the reason. An unknown key names no stored key and is not an event.
   - The admission that admits the host spends the key in its own transaction. From then on the key enrolls no host, its own included: a host that is reset, reinstalled or whose enrollment failed needs a new key or seed.
   - A host whose enrollment is refused polls once with its key's proof (`GET /v1/enroll/{hostID}`), and takes the certificate and configuration of an admitted answer. So an agent whose answer was lost and whose host was admitted meanwhile is not stranded, and a joined host that holds its certificate and no configuration polls for the configuration before it enrolls.
   - A fresh key that an admitted host presents is bound to that host and spent, so it enrolls no other.
9. **ACME is not here.** naru-proxy's certificate comes from ACME under garamsh/naru#395, and until then it is the file ADR 0120 item 3 names.

## Options considered

- **A KMS or OpenBao holds the authority keys.** Rejected by sauce (comment 19964427). It adds a service the signer must reach before it can sign. The `crypto.Signer` seam keeps it open for later.
- **No rollover: a person re-runs `init`, or the authorities live longer than ten years.** Rejected. A new key orphans every certificate already issued, and a longer life only postpones the day every pool breaks at once.
- **Rotate to a new key with a cross-signed overlap.** It also covers a compromised key. Not built now (#451): re-signing needs no change in what a pool trusts beyond taking a certificate, and the overlap needs a chain the consumers do not build yet.
- **Spend an enrollment key at its first presentation.** Rejected. The host's agent retries `POST /v1/enroll` on a transient failure, and a key spent at its first presentation would leave a host whose answer was lost unable to enroll at all.
- **One enrollment key for a batch of hosts.** Rejected by the PM. A leaked key would enroll any number of hosts, each pending until an operator admits it, and the pairing code would be the only check left.

## Consequences

- The authorities end no sooner than ten years after the last check that signed them again, provided a signer runs at least once in each two-year window.
- Until (b), (c) and (d) land, no pool refreshes, and the warning is the only sign of the end. A pool made at `init` is warned of from year 9.
- An operator makes one enrollment key per host, within the day the host boots. A key lost to a failed boot is revoked by its 24 hours, and a new one is made.
- `docs/operate/first-hosts.md` makes a key and a seed per host.
- `GET /v1/host` gains `trustExpiresAt`, and orch's events gain `authority.expiring`.

## Signs the decision was wrong

- A pool is still warned of after (b), (c) and (d) have landed: a holder's refresh misses a case, and the warning should become a refusal to start.
- An authority key is compromised: re-signing does nothing for it, and #451 is needed at once.
- Operators keep making enrollment keys a day ahead for hosts that boot later: then the life should be a parameter of the key, bounded by what the operator is allowed.
- A KMS becomes a requirement: then the `crypto.Signer` seam takes it, and the bundle no longer carries that key.
