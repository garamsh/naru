# ADR 0041: orch is a cluster of members with an embedded etcd, one elected leader and an mTLS API that enrolls and admits hosts, and the signer holds every key

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#118 (plan garamsh/naru#113). It builds on `lib/identity` (garamsh/naru#117): the roles, the pairing code, the admission statement, the revocation list and the sealed box are decided there and used here as they are.

## Context

- **A control plane above the hosts needs an inventory** of hosts that survives the loss of a control host, and a way to bring a host in that a person can check.
- **The authorities' keys must not be in orch.** A host that enrolls is admitted by an operator's signed statement and certified by the signer, so orch holds no key that can mint an identity.
- **Enrollment is open to the network** by design: a host has no certificate yet, only an enrollment key, so the endpoint needs its own limits.
- **Alternatives met:** an external etcd (one more thing to deploy and to secure on every control host); a lock without election (no single place to run reconcilers); orch trusting the pairing code a host sends (a host could then claim any code); a plain pin of the server certificate for hosts (it changes at every renewal).

## Decision

- **Each control host runs a member of `naru-orchd`: an embedded etcd v3.6 over mutual TLS, an election on it, and an HTTPS API with mutual TLS.** Reconcilers and derived state run only on the elected member; the lease is 10 s and a leader does not resign.
- **Roles come from `lib/identity`; orch maps them to five actions** (read, operate, administer, report, any). Automation may operate; only root may take a snapshot; only a host may heartbeat. A revoked serial is refused from the signer's list, which orch verifies with the revocation public key.
- **Enrollment is key-only and limited.** Keys are stored as SHA-256 only; a token bucket per source and per key answers 429; the buckets live in each member's memory.
- **Orch recomputes the pairing code from the stored key** and compares it with the operator's; the admission statement is opaque to orch and goes to the signer with the host's certificate request. The region and labels travel in the request beside the statement.
- **A host trusts orch by the SHA-256 of the authority's certificate and by orch's name,** the authority being in the chain orch presents.
- **Snapshots are written only sealed** to the recovery public key, every 6 hours, newest 8 kept, and on `POST /v1/snapshot` by root.
- **The signer's HTTP contract is assumed** (`POST /v1/host`, `GET /v1/crl`, `POST /v1/orch`, JSON fields `statement`, `csr`, `certificate`, `ca`) until the signer's document fixes it.
- **Extension packaging of orchd (garamsh/naru#57) is out of this decision.**

## Consequences

- **Easier:** the loss of one of three control hosts leaves the inventory and the API up; a leaked enrollment table gives no key; a compromised orch cannot issue an identity.
- **Harder:** three members hold etcd data and a peer network; a failover takes about the lease; a changed role set is a change in `lib/identity` and here.
- **Ruled out:** unsealed snapshots; a pairing code taken from a host's request; keys in orch.
- **Weaker than it could be:** the rate limits are per member, so three members allow three times the rate.

## Signs the decision was wrong

- Failover takes visibly longer than the lease, or two members act as leader at once.
- Hosts are refused because the authority's certificate was absent from orch's chain, or an enrollment flood passes the per-member limits and fills the pending list.
- The signer's real contract differs from the assumed one in more than field names.
- Operators keep a snapshot unsealed outside orch because restore is too hard.
