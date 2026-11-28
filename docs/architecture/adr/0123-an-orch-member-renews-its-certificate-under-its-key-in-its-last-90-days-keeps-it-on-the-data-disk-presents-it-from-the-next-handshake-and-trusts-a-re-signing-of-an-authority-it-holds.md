# ADR 0123: an orch member renews its certificate under its key in its last 90 days, keeps it on the data disk, presents it from the next handshake, and trusts a re-signing of an authority it holds

> Status: accepted
> Date: 2026-11-28

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#412, as part (c) of the six pull requests in the PM's decision 2 that [ADR 0121](./0121-the-signer-makes-and-holds-its-authorities-signs-each-again-under-its-key-two-years-before-it-ends-and-an-enrollment-key-enrolls-one-host-within-a-day-superseding-adr-0040-in-part.md)'s context records. It builds ADR 0121 items 4 and 7 for orch, as [ADR 0122](./0122-a-host-renews-its-certificate-under-its-key-in-its-last-90-days-retires-its-others-trusts-a-re-signing-of-an-authority-it-holds-and-pins-orch-by-its-authoritys-key-superseding-adr-0041-and-0082-in-part.md) built them for the host. It supersedes no record.

## Context

- **An orch certificate lives one year, and only root renewed it.** Root had the signer issue it (`POST /v1/orch`) and placed it in the stored configuration's `spec.orch.certificate`. A member whose certificate ended was refused by its peers, its hosts and its callers until root did that again.
- **init writes the member's pair at every boot.** It writes `/run/naru-orchd/orch.crt` and the authorities from `spec.orch`, and `orch.key` from `spec.files`, so a certificate renewed while orch runs has to be kept where a boot does not overwrite it, as hostd's store is (ADR 0122 item 4).
- **Five readers hold the member's pair, and each read it once, at start:** the API, the Machine API, etcd (on its peer and client ports), orch's own client of etcd, and its client of the hosts' APIs.
  - etcd 3.6 reads its certificate and key files again at every handshake, as a server and as a peer's client. It reads its trusted authorities' file once, at start.
- **The signer already holds what a renewal needs.** It issued each member's certificate, so its `issued.json` holds the serial with the role `orch`, and the certificate holds the DNS names and addresses root authorised.
- **The PM's decisions on the design (2026-11-28).** The design was approved as written, with these additions:
  - the renewed pair and the stores are written whole (a rename), the pair mode 0600 and the stores 0644, owned by orch; a torn write found at start falls back to `--cert`;
  - moving the rule of a re-signing to `lib/identity` keeps hostd's tests unchanged;
  - etcd's gap is an accepted risk, warned of through the file etcd trusts, and orch does not restart etcd;
  - a reset of the data disk falls back to the first pair.

## Decision

1. **A member renews its certificate under its own key.**
   - The member signs a request `renew-orch` (`identity.ActionRenewOrch`) with its current key and certificate. Its body (`identity.OrchRenewal`) is a certificate request for the same key. It posts the request to its own signer's `POST /v1/orch-renew`, over the unix socket.
   - The signer verifies the request against the orch authority and the revocation list, and requires the role `orch`. It refuses a request for another key with 400.
   - Under the lock every revocation takes, it checks again that the signing certificate is not revoked, and that `issued.json` records it as an orch certificate. A revocation that lands between the verification and the issue ends the renewal with 403.
   - The new certificate's CN is the member's name. Its DNS names and IP addresses are those of the current certificate, which root authorised. It carries server and client authentication and lives a year.
   - The signer records each member's serials in `orchs.json` (0600, optional, carried by the bundle). A member certificate issued before that file existed is recorded at its first renewal.
   - A certificate has at most one renewal the member has not retired. The renewal a lost answer left is revoked in the same commit as the new one's issue, as for a host (ADR 0122 item 1).
2. **The window is the last 90 days.**
   - `identity.OrchRenewWindow` is 90 days, and a renewal earlier than that is 409 (`identity.ErrOrchNotDue`).
   - Every member checks at start and every 24 hours. A 409 whose message is the not-due error changes nothing, and the next check renews; any other answer is a failed renewal.
   - A certificate that has ended renews nothing: root has the signer issue the member a new one, as before.
3. **The renewed certificate is kept on the data disk, and every reader reads it from there.**
   - The member keeps its pair in `--renewed-cert` (`/var/lib/naru/orch/renewed/orch.crt`, mode 0600): its certificate, followed by its authority's certificate that ends last. It keeps the key in `--key`, which a renewal does not rewrite.
   - At start the member keeps that file when it meets every one of these conditions:
     - it is a pair with `--key`;
     - it chains to an authority of `--ca` and the store (item 5) for both uses;
     - it has not ended;
     - the revocation list does not hold it;
     - it ends after `--cert`.
   - Otherwise it writes `--cert`'s content there. That covers the first start, a torn write, a reset of the data disk, and a new pair root placed in the configuration.
   - A list that cannot be had does not, by itself, replace a certificate that is otherwise good.
   - `--renewed-cert` may not be `--cert`, which init overwrites at each boot.
   - The API, the Machine API, orch's own client of etcd, and its client of the hosts present the pair as the files stand (`identity.FilePair`). Each reads the files again at a handshake when either has changed. When a torn write leaves them unreadable, each keeps the pair it last read.
   - etcd's certificate file is `--renewed-cert` too, so it presents the renewal from its next handshake.
   - The host client presents the renewed pair when `--host-client-cert` and `--host-client-key` are absent or name `--cert` and `--key`, as init gives them. A pair the operator names otherwise is presented as its files stand.
4. **The member presents the new certificate, then retires its others.**
   - The member checks the answer before it uses it: its own name and key, and a chain to an authority of `--ca` and the store for both uses. Anything else changes nothing.
   - It keeps the retire first: the new serial, in `orch.retire` beside `--renewed-cert`. Then it writes the new pair whole to `--renewed-cert`, which every reader presents from its next handshake.
   - It then signs `retire-own-orch` (`identity.ActionRetireOwnOrch`) with the new certificate and posts it to `POST /v1/orch-retire`. The signer revokes every serial of the member but the one that signed it. A repeat changes nothing, and a revoked certificate's retire is refused.
   - A retire that fails is kept and tried at each check, and does not stop the next renewal.
     - One kept for a serial the member does not present is dropped.
     - One the signer answers 409 for is dropped and logged: a serial it holds no record of, as after a restore from an older bundle.
5. **A member trusts an authority's re-signing, and never a new key.**
   - At each check the member reads its own signer's `GET /v1/ca`. It stores in `--ca-store` (`/var/lib/naru/orch/renewed/ca-store.pem`, 0644) only a re-signing of an authority `--ca`, `--host-ca` or the store already holds, by the rule ADR 0122 item 4 gives the host. That rule is now `identity.Resigns`, which hostd and orch share.
   - The API and the Machine API verify callers against `--ca` and the store together. The host client verifies the hosts against `--host-ca` (or `--ca`) and the store. Each reads them again at a handshake when one has changed (`identity.FilePool`, `hostclient.Transport`).
   - etcd trusts `etcd-ca.pem` beside `--renewed-cert`: `--ca` and the store as they stood when the member started, written at each start.
6. **etcd's trust is the store as it stood at start, and its end is warned of.**
   - etcd reads its trusted authorities once, so a re-signing taken while the member runs reaches etcd at its next start. Orch does not restart etcd.
   - The member's pools that `authority.expiring` warns of (ADR 0121 item 5) are `ca` (`--ca` and the store), `host-ca` (`--host-ca` and the store, when it is another file), and `etcd-ca` (`etcd-ca.pem`). A member that took a re-signing and has not restarted since is warned of `etcd-ca` a year before the old certificate ends.

## Options considered

- **A new key at each renewal.** Rejected, as for the host: the key is in `spec.files`, which root's configuration carries, and a new key would have to reach the configuration too. A compromised member key is a revocation and a new pair from root, as before.
- **Renew into `--cert`.** Rejected: init writes it afresh from the stored configuration at each boot.
- **Write the renewal into the stored configuration.** Rejected: the configuration is root's to change, and a member would need root's authority to change it.
- **Restart etcd, or the member, after a renewal.** Rejected: etcd reads its certificate at every handshake, so no restart is needed for the certificate. A restart for the trusted authorities would take a member out of the quorum at a time no operator chose, and the warning gives the operator a year to choose one.
- **Reload etcd's trusted authorities in place.** Rejected: etcd 3.6 gives no way to do it.

## Consequences

- A member renews itself every year, in its certificate's last 90 days, with no person involved. Its other certificates are revoked once the new one is presented.
- **A reset of the data disk falls back to the first pair.** After the member's first retire, the first pair is revoked. A member whose data disk was reset after it renewed starts with a revoked certificate, and is refused until root has the signer issue it a new one.
- **etcd's trust lags the store until the member restarts.** etcd keeps trusting the authority certificate it started with until that certificate ends. A member restarted within the year it is warned of takes the re-signing. One that is not restarted refuses its peers, and its peers refuse it, once that certificate ends.
- `naru-orchd` gains `--renewed-cert` and `--ca-store`, and the signer gains `POST /v1/orch-renew`, `POST /v1/orch-retire` and `orchs.json`.
- **A stolen member pair renews until the member is revoked.** As for the host, nothing is spent by a renewal. The bound is one unretired renewal per certificate, and a revocation of the member ends every renewal that has not been issued.

## Signs the decision was wrong

- A member is warned of `etcd-ca` and is not restarted within the year: the warning is not enough, and the member should restart its etcd at a time it chooses.
- Members start with the first pair after a renewal though their data disks were not reset: the start's checks refuse a good renewal.
- A member's old certificates stay unrevoked for long after a renewal: retires fail more often than the daily check recovers.
