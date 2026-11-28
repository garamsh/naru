# ADR 0122: a host renews its certificate under its key in its last 90 days, retires its others, trusts a re-signing of an authority it holds, and pins orch by its authority's key, superseding ADR 0041 and ADR 0082 in part

> Status: accepted
> Date: 2026-11-28

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#412, as part (b) of the six pull requests in the PM's decision 2 that [ADR 0121](./0121-the-signer-makes-and-holds-its-authorities-signs-each-again-under-its-key-two-years-before-it-ends-and-an-enrollment-key-enrolls-one-host-within-a-day-superseding-adr-0040-in-part.md)'s context records. It builds ADR 0121 items 4 and 7 for the host, and it supersedes in part:
- [ADR 0041](./0041-orch-is-a-cluster-of-members-with-an-embedded-etcd-one-elected-leader-and-an-mtls-api-that-enrolls-and-admits-hosts-and-the-signer-holds-every-key.md)'s "A host trusts orch by the SHA-256 of the authority's certificate": the pin becomes the SHA-256 of the authority's key;
- [ADR 0082](./0082-a-host-resets-its-store-its-data-disk-or-both-through-post-v1-reset-at-its-next-boot-and-a-refused-host-serves-maintenance-mode-to-operators-under-orchs-host-ca.md) item 7's "taking the chain entry whose SHA-256 matches": maintenance mode takes every chain entry of the pinned key.

The rest of both records stands, and neither body is edited.

## Context

- **A host certificate lives one year, and nothing renewed it.** A host whose certificate ended could not report to orch, and was enrolled again from a new key and seed.
- **A pin of a certificate's hash does not survive a re-signing.** The signer signs each authority again under the same key two years before it ends (ADR 0121 item 3). Every pin of orch's authority names one certificate of it, so a host pinned to the old one refuses orch once orch presents the new chain. The pin is in five places:
  - hostd's `--orch-ca-sha256`;
  - the install seed's `spec.join.orch.caSHA256`;
  - a configuration's `spec.host.orch.caSHA256`, which `naructl gen host-profile` writes into the fleet's profile;
  - hostd's maintenance mode, which takes orch's authority from the pinned chain (ADR 0082 item 7);
  - the line `naructl signer ca` and `naructl orch ca` print, and the console's install-seed form.
- **hostd's `--client-ca` is written by init at each boot** from the stored configuration, under `/run/naru-hostd/`. A pool that is refreshed on a running host has to be kept somewhere a boot does not overwrite.
- **The PM's decisions on the design (2026-11-28).** The design was approved as written, with two additions:
  - the certificate-hash pin is kept for compatibility only, and is dropped in a named release, `v0.1.0-rc.1`, with a release note telling operators to re-render seeds and profiles before it;
  - the authorities are read only over the orch connection the key pin already checked, and a test shows that a certificate with a new key under a known subject is never added.
- **The Node API's `spec.api.clientCA`** is not the signer's authority and is not refreshed here: it moves with part (f) (the PM, 2026-11-28, noted on #412).

## Decision

1. **A host renews its certificate under its own key.**
   - hostd signs a request `renew-host` (`identity.ActionRenewHost`) with its current key and certificate. Its body (`identity.HostRenewal`) is a certificate request for the same key. hostd posts it over mTLS to orch's `POST /v1/hosts/renew`.
   - Orch passes it to the signer's `POST /v1/host-renew` only when the certificate that signed it is the one the host connects with, byte for byte. It also checks that the action is `renew-host`. Anything else is 403, and a body that is not a signed request is 400. Orch verifies nothing else: the signer does.
   - The signer verifies the request against the host authority and the revocation list, and requires the role `host` and a host ID for subject. It refuses a request for another key with 400.
   - Under the lock every revocation takes, it checks again that the signing certificate is not revoked and is one of the host's serials. A revocation that lands between the verification and the issue ends the renewal with 403, so no certificate is issued past it.
   - The new certificate's CN and DNS name are the host ID, its IP addresses are those of the current certificate, and it lives a year. Its serial is recorded in the signer's `hosts.json`.
   - A certificate has at most one renewal the host has not retired. A host whose answer was lost renews again, and the renewal it lost, a serial recorded after the signing one and not revoked, is revoked in the same commit as the new one's issue.
2. **The window is the last 90 days.**
   - `identity.HostRenewWindow` is 90 days. A renewal earlier than that is 409 (`identity.ErrHostNotDue`), which orch answers as 409 too.
   - hostd checks at start and every 24 hours (`enroll.Config.CheckEvery`). A 409 whose message is the not-due error, from a signer whose clock is behind, changes nothing, and the next check renews; any other answer is a failed renewal.
   - A certificate that has ended renews nothing: the host is enrolled again, from a new key and seed (ADR 0121 item 8).
3. **The host serves and reports with the new certificate, then retires its others.**
   - The agent checks the answer before it uses it: the host's own name and key, and a chain to an authority of `--client-ca` and the store, for client authentication. Anything else changes nothing.
   - The agent keeps a retire before anything changes: the new serial in `host.retire` beside the certificate. A crash once the host reports with the new certificate leaves the retire to the next check.
   - The host API serves the new pair first (`api.Serving.Replace`, stored beside the host key as `serving.pem`). Then the agent heartbeats to orch with it, and writes it beside the host key. The key is not rewritten.
   - It then signs `retire-own-host` (`identity.ActionRetireOwnHost`) with the new certificate and posts it to orch's `POST /v1/hosts/retire`. Orch passes it to the signer's `POST /v1/host-retire` under the same check as item 1.
   - The signer revokes every serial of the host but the one that signed the retire. A retire repeated changes nothing. A revoked certificate's retire is refused.
   - A retire that fails is kept and tried at each check until one is done, and does not stop the next renewal. One kept for a serial the host no longer reports with, as an operator's rotation (ADR 0077) or a renewal that stopped before it reported leaves it, is dropped. One the signer answers 409 for, a serial it holds no record of as after a restore from an older bundle, is dropped and logged, since no retry can do it.
   - A rotation and a renewal are serialised: each waits for the other to finish.
   - A host API that cannot serve the new pair stops the renewal before anything is retired. The retire would revoke the pair the API serves, and the host would refuse to serve with it. The next check renews again.
4. **A host trusts an authority's re-signing, and never a new key.**
   - Orch serves `GET /v1/ca` to the role `host`: the signer's authorities, which hold nothing secret.
   - hostd reads it at each check over its orch client, whose connection the key pin checked (item 5), and no other way.
   - hostd stores a certificate in `--client-ca-store` (`/var/lib/naru/hostd/client-ca.pem`, on the data disk) only when it meets every one of these conditions:
     - it is an authority's;
     - it has the subject and key of an authority that `--client-ca` or the store already holds;
     - it is self-issued, and its signature verifies under its own key, which is that authority's: a certificate of the authority's name and key that another key signed is a forged root, and is never stored;
     - it lives no longer than the signer signs an authority for, `identity.AuthorityYears` (10) and a day;
     - it ends after every certificate of that authority held.
   - A certificate of any other key is never stored, under whatever subject, and neither is one of a known key under another subject.
   - The host API verifies its clients against `--client-ca` and the store together. It reads both again at a handshake when either has changed. A host importing a peer's disk verifies the peer against the same pool.
   - `GET /v1/host`'s `trustExpiresAt` (ADR 0121 item 5) is read from both.
5. **Orch's authority is pinned by its key.**
   - Every pin of orch's authority is the hex SHA-256 of the authority's public key, PKIX DER (`orchapi.KeyPin`). That covers hostd's `--orch-ca-sha256`, `spec.join.orch.caSHA256`, `spec.host.orch.caSHA256` and the fleet's profile. A re-signing keeps the key, so it keeps the pin.
   - `orchapi.PinCA` and maintenance mode take as roots every certificate of the chain orch presents that has the subject and key of the pinned one (`orchapi.PinnedRoots`), and verify orch's certificate against them. A certificate under the pinned name with another key is not a root.
   - `naructl signer ca` and `naructl orch ca` print `# <name> authority, key SHA-256 <hex>`, and the console's install-seed form fills the key pin.
   - The flag and field names do not change.
   - **The certificate-hash form is kept for compatibility only, and is dropped in `v0.1.0-rc.1`.** Until that release a pin that is the SHA-256 of an authority's certificate is still accepted, as before. From it, only the key pin is. Operators re-render their seeds and profiles with the key pin before then. garamsh/naru#457 tracks the drop.
6. **The Node API's `spec.api.clientCA` is not refreshed here.** That authority is not the signer's yet, and it moves into the signer with part (f) of #412.

## Options considered

- **A new key at each renewal.** This is what a caller does (ADR 0116). Rejected for the host: its key is what orch's pending record, the pairing code and the join ID were derived from, and a host that changed keys would have to be admitted again. A compromised host key is a revocation and a new enrollment, as before.
- **Orch revokes the old serial when the host first heartbeats with the new one.** Rejected: orch would need the signer's revocation call under its own authority, and a host whose new pair failed to apply would lose the old one too. A retire the new certificate signs proves that the host holds and uses it.
- **Refresh the trust pool in place, in `--client-ca`.** Rejected: init writes it afresh at each boot from the stored configuration, so the refresh would be lost at the next boot.
- **Take every authority `GET /v1/ca` answers.** Rejected: a host would then trust whatever an orch it reaches serves. Taking only a re-signing of a key it already trusts means orch cannot add trust it did not have.
- **Drop the certificate-hash pin now.** Rejected: every seed and profile already written holds one, and a host booted from one would refuse orch. The named release bounds how long the old form lingers.

## Consequences

- A host renews itself every year, in its certificate's last 90 days, with no person involved. Its old certificates are revoked once the new one serves and reports.
- A host's trust in the signer's authorities outlives their certificates. The 2-year window of ADR 0121 item 3 is covered for the host's `--client-ca` and its pin of orch.
- **The pin accepts a forged root for one connection.** A certificate with the authority's real key and subject, in a certificate an attacker made, is accepted as a root of that connection only: its validity and its constraints. Every certificate under it is still verified by the authority's signature, which only the key can make. The store never keeps one (item 4).
- **A stolen host pair renews until the host is revoked.** Nothing is spent by a renewal; the bound is one unretired renewal per certificate (item 1), and a revocation of the host ends every renewal that has not been issued.
- `naru-hostd` gains `--client-ca-store`, orch gains `POST /v1/hosts/renew`, `POST /v1/hosts/retire` and `GET /v1/ca`, and the signer gains `POST /v1/host-renew` and `POST /v1/host-retire`.
- Seeds and profiles written before this record keep working until `v0.1.0-rc.1`.
- A renewal revokes a serial recorded after the one that signed it. An operator's rotation (ADR 0077) whose new certificate the signer has issued and the host has not yet taken is such a serial, so a renewal in that gap revokes it, and the rotation's `PUT /v1/identity` is refused by orch's heartbeat and is done again.

## Signs the decision was wrong

- A host is warned of (`authority.expiring`) for its `--client-ca` though the signer re-signed its authorities: the store missed a case.
- A host's old certificates stay unrevoked for long after a renewal: retires fail more often than the daily check recovers, and orch should record a pending retire itself.
- Hosts renew outside their window, or many hosts renew on the same day: the window or the check needs jitter.
- The certificate-hash form is still in use at `v0.1.0-rc.1`: the release note was not enough, and init should refuse it with a message first.
