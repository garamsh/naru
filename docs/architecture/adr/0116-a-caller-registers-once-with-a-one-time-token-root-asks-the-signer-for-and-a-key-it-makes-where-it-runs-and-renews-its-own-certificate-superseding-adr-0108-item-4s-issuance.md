# ADR 0116: A caller registers once with a one-time token root asks the signer for and a key it makes where it runs, and renews its own certificate, superseding ADR 0108 item 4's issuance

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#409, in goal garamsh/naru#378. It supersedes the part of [ADR 0108](./0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md) item 4 that says who the caller is and how it gets its certificate ("Issuance", and the Consequences line on renewal). The rest of ADR 0108 item 4 stands: the role, the certificate's OU and CN, what a caller may do, and the other roles' refusal. Its body is not edited.

## Context

- **The owner's direction.** In garamsh/discussions discussion 6 (comments 19856107 and 19857399), the owner set three rules for the places that run garam's work:
  - each execution place has one connector, registered once;
  - connections are pull only, and garam never dials into a place;
  - no key is made, placed or renewed by a person.
- **The PM's decision on #409 (2026-11-24).** The Machine API's caller is garam-bot's workspace provider, which runs at the execution place: the cluster connector, or the naru-host connector. garam only declares. The caller registers once with a one-time registration token and a key it generates where it runs, and the key never leaves. It renews its own certificate before expiry, in the shape of garam-bot-operator ADR 0088 and ADR 0093.
- **What ADR 0108 decided instead.** Item 4 makes garam the caller, with a 90-day certificate that root issues by hand (`naructl caller credential`) and garam keeps in AWS Secrets Manager. Its Consequences say that nothing renews a caller automatically. Both break the owner's third rule.
- **The precedent next door.** garam-bot-operator ADR 0088 (at `404ef0e` of that repository) registers its connector with a token that lives one hour and is good for one attempt, "spends it whatever the outcome", and binds a key the connector made in its own process. Its ADR 0093 makes that the rule for every cluster. GitHub Actions runners register the same way.
- **What the signer is.** It listens on a unix socket on the control hosts, and nothing outside a control host reaches it ([signer.md](../signer.md)). There may be several signers, replicas that share the authority keys through the bundle and the revocation list through orch's sync. A signer keeps a nonce once on that list until the nonce's expiry, which is how a reissue of root is taken only once ([ADR 0052](./0052-a-reissue-nonce-is-signed-into-the-revocation-list-so-orchs-sync-carries-it-to-every-signer-and-a-peered-signer-refuses-a-reissue-until-it-has-taken-the-list-lately.md)).
- **What a caller reaches.** Only orch's Machine API listener (ADR 0108 item 3), and that listener serves no other route.

## Decision

1. **The caller is the use's provider at the execution place.** For garam it is garam-bot's workspace provider: the cluster connector, or the naru-host connector. garam declares workspaces and holds no caller credential. The name a caller registers under is the one root gives its token, a DNS label as before.

2. **Root asks the signer for a one-time registration token.**
   - The call is `POST /v1/caller-token`, root only, through `naructl caller token --name --admin --passphrase-file --out [--ttl]`. Its body is the caller's name and `ttlSeconds`.
   - A token lives one hour by default and 24 hours at most.
   - The token is `naru-ct-`, then the claims (name, a random nonce, expiry) and the operator authority's ECDSA signature over a domain string and the claims, both base64url. Every replica holds the operator authority, so any signer takes a token any signer made. The signer keeps no copy, and the log names the nonce, never the token. `lib/identity` reads a token's claimed nonce, unverified, so that orch's log can name it too.
   - naructl writes the token to `--out` only (mode 0600). It is a secret until it is spent or expires.

3. **A caller registers once, with the token and a key it made.**
   - The call is the signer's `POST /v1/caller-register`, with the body `{token, csr}`. The token is the only authentication.
   - `POST /v1/caller`, which root's direct issuance answered before this decision, is not given a new meaning in place: it answers 410 and names the two paths that replaced it, so a naructl older than its signer says so rather than registering or issuing by accident.
   - A token that is malformed, signed by another authority, changed, expired or spent gets one answer, 403, which says nothing of why.
   - The signer signs the token's nonce into the revocation list before it reads the certificate request, so presenting a valid token spends it whatever the outcome.
   - A peered signer that has not taken a list in the last 30 seconds answers 409 and keeps the token, as it does for a reissue of root. It answers so before it reads the token, so a 409 says nothing of whether the token is valid.
   - The signer logs a refused registration with the nonce the token claims, and a registration with the token's nonce and the serial issued.
   - The answer is a caller certificate for the request's key: OU `caller`, CN the token's name, client authentication only, valid 90 days. Its serial is recorded as role `caller`.

4. **A token is taken once on every signer that has the list.** The nonce reaches the other signers by orch's sync, and from then on they refuse the token.
   - **Accepted risk:** within the sync interval after the first use, a second presentation on a signer the list has not reached still registers. This is the risk ADR 0052 accepts for a reissue of root.
   - The window is bounded by the sync interval and by the 30-second rule of item 3. Whoever holds a copy of a token could register before its owner anyway.
   - A token spent twice shows as one `token_nonce` in the "registered a caller" lines of two signers. Revoking the name (item 8) and registering again with a new token ends both certificates.

5. **A caller renews its own certificate, in the last 30 days of its life.**
   - The call is `POST /v1/caller-renew`, signed with the caller's current certificate, whose role must be `caller`. Its body is the certificate request of a new key the caller made.
   - The name is the current certificate's.
   - A renewal is refused:
     - before the last 30 days of the current certificate (409);
     - from a revoked or expired certificate, or one of a caller revoked by name (403, by the signed-request check every call makes, which is `lib/identity`'s `Authenticate`);
     - for the current certificate's own key (400), since a renewal with it would keep a key alive past the 90 days a certificate bounds it to;
     - from a certificate that has renewed already (403).
   - **A certificate renews once.** The signer signs `renew:<the current certificate's serial>` into the revocation list as a nonce until that certificate expires, before it issues, as a token is spent. Without it, whoever copied a caller's pair could renew it as many times as it liked, each successor valid 90 days and renewable in turn, and revoking the serials one signer knew would not reach the rest.
   - The new certificate is valid 90 days. The current one is not revoked, so the caller moves to the new pair once it holds it. An old key therefore stays valid at most 30 days after the key that replaces it.
   - A caller that loses the answer to its renewal holds a valid certificate that renews no more. It registers again with a new token before the certificate expires.
   - **Accepted risk:** within the sync interval after a renewal, a signer the list has not reached takes a second renewal of the same certificate, as it takes a second presentation of a token (item 4).
   - No person is involved.

6. **Root's direct issuance stays, for tests and recovery.** It moves from `POST /v1/caller` to `POST /v1/caller-issue`, and `naructl caller credential` calls it unchanged. A caller that has lost its key registers again with a new token instead.

7. **The caller reaches registration and renewal through the Machine API listener.**
   - `POST /machines/v1alpha1/register` takes no client certificate. It is the one route the listener serves without one: a request without a verified client chain to any other path, one the API does not serve among them, is 403 before any handler runs.
   - Registration is limited in each member's memory, 429 above either limit:
     - per source, as enrollment is (a token bucket of 1 per second, burst 10), where a source is the IPv4 address or the IPv6 /64, since one IPv6 host holds every address of its /64;
     - for every source together, by one bucket of 1 per second, burst 30, so that many sources cannot spend the signer's work;
     - the per-source buckets are at most 4096; a new source when every bucket is held and none has refilled is refused, not let through.
   - Orch logs a registration it limits, and one the signer refuses, with the source and the nonce the token claims.
   - `POST /machines/v1alpha1/renew` takes the caller's certificate.
   - The member that takes either call passes it to its own signer's socket and adds no authority. The token, or the request the caller signed, is what the signer checks, as for a host's certificate.
   - A caller signs its renewal with `lib/machineapi/signedrequest`, the form of `lib/identity`'s signed request in a package of the standard library alone, so a caller outside naru takes `lib/machineapi` without any other naru module.

8. **Root or an operator revokes a caller by name.**
   - The call is `POST /v1/revoke-caller` (body the name), through `naructl caller revoke --name`.
   - The revocation list gains `callers`: each revoked name with a time. Every caller certificate of that name whose NotBefore is at or before the time is revoked, on every signer and every orch member, by `lib/identity`'s `Authenticate`. The time is the NotBefore a certificate issued at the revocation has (the signer backdates one minute), so a later registration under the name is valid.
   - The entry reaches certificates no record of the revoking signer holds, a successor another signer issued among them. The signer also puts on the list the serials it recorded under the name, each with the serial it renewed from (`callers.json`, carried by the bundle), and answers that lineage.
   - Orch's sync carries the entry, and of two times for one name the later stands.
   - **Accepted risk:** within the sync interval after the revocation, a signer the list has not reached still renews a certificate of the name. It is the window of item 4, and of ADR 0052.

## Options considered

- **The signer keeps each token, or its hash.** Every replica would need the record, and the bundle carries one only at a join. A token made on one signer would then not register on another. Signing the token with an authority every replica already holds needs no new record.
- **Root signs the token itself, offline**, as `authorise-replica` does. Nothing would be logged where the tokens are made, and #409 asks that the signer issue them.
- **Renewal at any time, and the old certificate revoked at renewal.** If the answer to a renewal were lost, the caller would hold only a revoked pair, and would need a person to register it again. The 30-day window bounds the overlap instead.
- **Renewal with the same key.** It would keep a key alive past the 90 days a certificate bounds it to. The signer refuses a renewal for the current certificate's key.
- **Renewal as often as asked, revoked by serial.** A copied pair would yield successors without bound, and each signer knows only the serials it issued. Spending the serial on the list bounds a pair to one successor on every signer the list reaches.
- **Revoking a caller by the serials every signer recorded,** with the records carried by orch's sync. The sync carries the revocation list and nothing else, and a record would have to reach every signer before a revocation could name a successor issued elsewhere. One signed entry for the name revokes them all where the list already goes.
- **garam as the caller, as ADR 0108 had it.** garam would hold a credential for every place, a person would carry it, and garam would be the side that dials. That breaks the owner's rules.

## Consequences

- A use's provider gets its first certificate from a token a person copies once, and renews it with no person after that. No private key is made, placed or carried by hand.
- The signer gains `POST /v1/caller-token`, `POST /v1/caller-register`, `POST /v1/caller-renew` and `POST /v1/revoke-caller`, root's issuance moves to `POST /v1/caller-issue`, and `POST /v1/caller` answers 410. naructl gains `caller token` and `caller revoke`. `lib/identity`'s `Identity` carries the certificate's expiry and public key, and its revocation list the revoked caller names.
- A caller that loses its renewal's answer needs a person and a new token, which the Options considered meant to spare it; the window to retry is the rest of its certificate's 30 days.
- Orch's Machine API listener serves two routes besides `/machines/v1alpha1/`'s resources.
- garam's Workspace controller holds no caller pair. [machines.md](../machines.md) §The first user says what holds it instead.

## Signs the decision was wrong

- Callers lose their keys, or their renewals' answers, often enough that registering again with a new token is routine.
- The sync interval proves long enough that a token spent twice on two signers is seen in practice.
- A caller cannot keep its key across the last 30 days of its certificate, for example because its place is down for longer, and has to register again each time.
