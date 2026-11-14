# ADR 0052: a reissue nonce is signed into the revocation list so orch's sync carries it to every signer, and a peered signer refuses a reissue until it has taken the list lately

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#117 (plan: garamsh/naru#113), from the review of garamsh/naru#141, risk 1. It supersedes item 3 of [ADR 0051](./0051-the-recovery-key-signs-a-once-only-request-for-a-new-root-certificate-and-the-signer-revokes-every-root-serial-it-recorded.md) and its accepted risk of a restored or replicated signer with no record of the nonces; the rest of 0051 stands. Not an appended note on 0051: the decision about where the nonce is kept changes, which a note is not for (`docs/architecture/README.md` §Rules, "ADRs are append-only").

## Context

- **A nonce kept in one signer's directory guards that signer only.** ADR 0051 kept `reissue-nonces.json` per signer and left it out of the bundle. A request captured during its ten minutes could be replayed on another replica, and a replay revokes the root the reissue just issued: a denial of service on the holder of the recovery key.
- **The revocation list already travels to every signer.** Orch's sync ([ADR 0041](./0041-orch-is-a-cluster-of-members-with-an-embedded-etcd-one-elected-leader-and-an-mtls-api-that-enrolls-and-admits-hosts-and-the-signer-holds-every-key.md)) stores the highest-sequence list in etcd and pushes it to each signer, which merges what it is given. The list is signed by the revocation key every signer holds, and it is in the bundle.
- **A sync takes time.** A nonce a signer used reaches another after two sync intervals at most (the member stores the list, then another member pushes it), 20 seconds by default. In that time the other signer does not know it.
- **No signer can tell it holds the newest list.** The socket is HTTP with no authentication of the caller beyond the file mode, and `POST /v1/crl` accepts a list the revocation key signed from whoever reaches it, so a push is no proof that orch made it.

## Decision

1. **A used nonce is part of the signed list.** `identity.CRL` gains `Nonces`, a map from nonce to the expiry of its request, and `Used(nonce, now)`. A reissue signs the list once, at the next sequence, with the revoked root serials and the nonce, and writes it before it issues. A list is carried with the nonces whose request has not expired; an expired one is dropped at the next signing, because its request fails on its expiry. `reissue-nonces.json` is removed.
2. **The sync is unchanged.** Orch's `crlsync` stores and pushes the signed list as bytes and compares sequence numbers, so it carries the nonces with the serials. A list that differs from a signer's own only in a live nonce is new to it, and a nonce-only change still rises the sequence because the reissue signs the list.
3. **A signer merges nonces like serials.** `POST /v1/crl` returns the list held and changes nothing when the pushed list adds no serial and no live nonce. A higher list is adopted only when it holds every serial and every live nonce the signer holds; otherwise the two are merged and signed at the higher sequence plus one. A signer refuses a reissue whose nonce is on its list and not yet expired, from any source: a reissue it made, a list a peer pushed, or the bundle it was restored from, which closes ADR 0051's accepted risk of a restored signer.
4. **The window is closed by a rule on the replica that has not heard from orch, not by naming a writer.** A signer that has taken a list through `POST /v1/crl` at least once is peered: it writes an empty `peered` file in its directory, and from then on refuses a reissue with 409 unless it took a list within the last 30 seconds (three of orch's default sync intervals). After a restart it has taken none, so a peered signer waits for the next push. A signer never pushed to is alone and is guarded by its own list, which is what a single-signer deployment is.
5. **Why not accept a reissue only on the replica orch names as the CRL writer.** Naming one needs orch to tell the signer, which is a new call from orch's leader, a change to its client and its composition site, and a second source of truth for who writes. It also closes less than it appears to: a failover of the leader inside the sync interval reopens the same window, and the signer cannot verify who made the call (decision 4's reason). The freshness rule needs no change to orch and fails closed when orch cannot reach the signer.

## Accepted risks

- **The window is narrowed, not closed, against a caller who reaches a signer's socket.** A signer's freshness is a push, and anyone who reaches the socket can push a list the revocation key signed, such as the signer's own, and so can keep a signer fresh. A replay by such a caller on a second signer during the sync interval after the first use still succeeds. What the rule guarantees is that a signer orch cannot reach does not take a reissue, and a signer that restarted does not, until orch has spoken to it. Closing the rest needs the caller of `POST /v1/crl` authenticated, which no decision has made.
- **The replay costs the root just issued, as ADR 0051 says.** The holder of the recovery key signs a new request.
- **A peered signer with orch stopped cannot reissue root after 30 seconds.** A reissue is for a lost root person file, and a cluster with no orch has a bigger loss; the signer's own directory is unchanged and `restore` from a bundle does not depend on the rule.
- **The `peered` file is made by any list pushed.** Anyone who reaches the socket can put a signer in the state that waits for orch. It refuses more, never less, and orch's next push lifts it.

## Consequences

- `lib/identity` gains `CRL.Nonces` and `CRL.Used`; `host/internal/signer` loses the nonce file, gains the freshness rule (`ErrNotSynced`, 409) and the merge of nonces in `PushCRL`; `orch/internal/crlsync` changes nothing and is tested with a list that carries a nonce.
- The signed list grows by at most the requests of ten minutes, each a 32-character nonce and a time, and only the recovery key's holder makes them.
- `orch/internal/orchtest`'s `CRLReplica` merges serials only. A test that wants nonces through orch's loop uses a replica that merges them.

## Signs the decision was wrong

- A captured request is replayed on a second signer and taken: the sync interval is longer than 30 seconds in a deployment, or the freshness rule needs authenticated pushes.
- A reissue is refused on a healthy deployment: the 30 seconds are shorter than orch's sync under load, and the constant belongs with orch's interval.
- The list's size matters: the nonces want their own record signed by the same key, as a sibling of the list.
