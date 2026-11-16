# ADR 0062: every naru TLS peer is checked against the signed revocation list by one verifier in lib/identity and fails closed when the list is older than its max age

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#176, part of goal garamsh/naru#197. It extends [ADR 0052](./0052-a-reissue-nonce-is-signed-into-the-revocation-list-so-orchs-sync-carries-it-to-every-signer-and-a-peered-signer-refuses-a-reissue-until-it-has-taken-the-list-lately.md) (the list and its max age in orch) and [ADR 0061](./0061-a-host-certificate-names-the-host-id-and-orchs-host-client-accepts-the-address-or-the-host-id.md) (the name rule of orch's host client), and does not change them.

## Context

- **Only orch checked the revocation list.** Orch's API verified a client certificate's serial against the signer's list. naru-hostd, the Node API and orch's own host client verified the chain and, for the host client, the name, and never the list. A revoked operator, orch or host certificate was still accepted by every one of them until it expired.
- **The chain and name check existed twice.** `lib/orchapi`'s `PinCA` and orch's `verifyHost` each wrote the chain verification and the name rule.
- **A service that checks a list needs a source for it** and a rule for what it does when the source is gone. A host is not the signer's neighbour: only a control host runs one.

## Decision

**One verifier in `lib/identity` checks the chain, the name rule and the revocation list, and every naru TLS peer that has a revocation key uses it. A service fails closed when it has no list or its list is older than the max age.**

1. **The verifier.** `identity.PeerCheck` verifies a peer's chain against the roots for a usage, applies the name rule of ADR 0061 when it is given names, and asks a `RevocationList` about the chain. `identity.RevocationList` holds the verified list, the source, the revocation public key, the TTL of a cached read and the max age. It is a `VerifyConnection` for a server (the client certificate) and for a client (the server certificate). `PinCA` and orch's host client call `PeerCheck`; `verifyHost` is gone. `Authenticate` calls the same chain check (`CRL.CheckChain`).
2. **The source.** A Node with `spec.signer` reads its local signer socket, `GET /v1/crl`. Every other host fetches orch's `GET /v1/crl` over `PinCA`, from `spec.host.orch` (the URL and `caSHA256`), and verifies the list with the revocation public key. **Orch's `GET /v1/crl` answers without a client certificate**: the list is signed and public, and a host that is to refuse a revoked peer must be able to read it without being one.
3. **Max age and failure mode.** The max age is `identity.DefaultMaxAge`, 10 minutes, as in orch, and is configurable in `spec.host.revocationMaxAge` and `spec.api.revocationMaxAge` (1 minute to 24 hours). With no list, or a list whose last successful refresh is older than the max age, every peer is refused: the handshake fails. A list refreshed within the max age stays in force while a refresh fails, and only until the max age passes. One caller at a time fetches, outside the lock, and a fetch started holds the next off for `identity.DefaultRetryInterval` (10 seconds); a handshake that finds a fetch running or failed lately is answered from the list held and never waits for the source.
4. **No rollback.** A list with a lower `seq` than the one held is refused, as is a list not signed by the revocation key. The last verified list is kept on the data disk (`/var/lib/naru/hostd/crl.json` for naru-hostd, `<data>/api/crl.json` for the Node API), so a reboot within the max age needs no fetch first.
5. **Admission.** A host that joined from a seed gets `spec.host.revocationPublicKey` and the current list as `spec.host.revocationList` in its admission configuration (`lib/layout` and orch's admission path). Orch reads the list from its signer when it plans the admission; a signer that cannot answer fails the admission. The list seeds the kept file, so a new host is not locked out before its first fetch; a seed is as old as its signed `issuedAt`, so one older than the max age with no source to refresh it is refused. The seed is verified in configuration validation.
6. **Without a key.** A Node with no `revocationPublicKey` checks no revocation, as before, and logs that at start. `PinCA` clients with no key (`naru-hostd enroll`, naructl) keep only the chain and the name check.
7. **The Node API's source is read at start.** A reload changes the key and the max age and keeps the source; a key with no source is refused.
8. **Metrics.** The list's age, sequence and last fetch error are exposed: naru-hostd `naru_hostd_revocation_list_*`, init `naru_init_revocation_list_*`.

## Consequences

- **Lockout analysis.**
  - A control host runs the signer and reads it locally, so its Node API and naru-hostd stay reachable while orch is down; orch is restored through the control hosts.
  - A worker host refuses its callers only from the time its list is older than the max age until orch answers again. Its microVMs keep running; only the calls to its API are refused.
  - A signer that stops leaves a control host's services on their last list for the max age, then refusing.
- **A revoke takes effect within the signer's sync interval, the TTL of the list's cache and a fetch**, not at certificate expiry.
- **Orch's API has one route more without a client certificate.** The route answers a signed list only.
- **New edges between modules:** `lib/orchapi` and `lib/layout` import `lib/identity`; `lib/init` imports `lib/orchapi`. Units still import no unit.
- **Kubernetes certificates inside a Node are not checked.**

## Signs the decision was wrong

- A worker host locked out by an orch outage often enough that operators raise the max age to hours, which would say a pushed list, not a pulled one, is the design.
- A host whose list is read from orch while orch's own certificate is the one being revoked and the revocation cannot reach the host, which would say a second source is needed.
- Two services on one Node holding lists that disagree.

## Erratum 2026-11-16: a host configured up front takes the key from its own configuration

Decision 5 gives a key to a host that joined from a seed. A host configured up front (`gen node --host-region`) that enrolls with `spec.host.orch` is admitted without a new configuration, so it had no key and checked no revocation (decision 6). `gen node` takes `--host-revocation-public-key <file>`, which writes `spec.host.revocationPublicKey`; with `spec.host.orch` it fetches the list from orch as decision 2 says. Init refuses the key without `spec.signer` or `spec.host.orch`.

Orch's `GET /v1/crl` answered the list as a JSON string of base64 instead of the signed list, which `lib/orchapi`'s `CRL` returns as read; a host fetching it from orch could not parse it. The route now writes the signer's bytes as they are.
