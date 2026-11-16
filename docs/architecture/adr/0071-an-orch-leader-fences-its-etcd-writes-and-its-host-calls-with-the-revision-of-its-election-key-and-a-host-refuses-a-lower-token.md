# ADR 0071: an orch leader fences its etcd writes and its host calls with the revision of its election key, and a host refuses a lower token

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#182, part of goal garamsh/naru#197. It adds to [ADR 0041](./0041-orch-is-a-cluster-of-members-with-an-embedded-etcd-one-elected-leader-and-an-mtls-api-that-enrolls-and-admits-hosts-and-the-signer-holds-every-key.md), which elects the leader, and to [ADR 0028](./0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md), which gives hostd its API.

## Context

- **A leader that lost its lease kept acting until its context was cancelled.** A member paused past the lease (a stopped process, a stalled VM) resumes with its lead context not yet cancelled, in the window before its session reports the loss. Another member already leads. Both write to etcd and call hosts, and the old one's writes can undo the new one's.
- **A lease says who leads now, not who may still act.** Only the store that is written, and the host that is called, can refuse the old leader, and neither knew which term a call came from.
- **The fence renewal is the sharpest case.** A stale leader that renews a fence extends the life of a stateful microVM another leader has already moved.

## Decision

1. **The token of a term is the CreateRevision of the member's election key.** etcd's revision only grows, and a later leader's key is created at a later revision, so a later term has a higher token. The token is read from the election the member won (`Election.Rev`).
2. **Every etcd write the leader makes as leader is guarded.** The engine's group and member writes, the events it appends, the prefixes and the inventory writes of the leader's own components go through `leader.Guard`: a transaction that compares the election key's CreateRevision to the token and, only then, runs the caller's own transaction as its `Then`. A caller's compare-and-swap that fails is still reported as the caller's failure. A guard that fails ends the term (its context is cancelled with `ErrTermLost`) and is not retried. The term travels in the context of the lead call (`leader.NewTerm`, `leader.TermFrom`); a guarded write with a context that carries no term fails.
3. **Writes of an operator's request are not guarded.** Any member serves the API, and a write there is a request of an operator under the compare-and-swap it already has, not a write of the leader. The API handlers keep the plain client.
4. **Every mutating call from orch to a host carries the token** in the header `X-Naru-Leader-Token` (`wire.LeaderTokenHeader`, in `lib/wire`), the fence renewal included. The header is added by a transport in `orch/internal/hostclient` from the term in the request's context, for any method but GET and HEAD. A call with no term in its context, which is an operator's, carries none.
5. **A host refuses a token lower than the highest it has seen**, with 409 and the code `stale-leader-token` in the error body (`hostapi.ErrorBody.Code`). It writes a higher token to a file, `--leader-token-file` (default `/var/lib/naru/hostd/leader-token`), with `lib/atomicfile` before it acts on the call, so a restart does not forget it. A call without the header is not checked, and a read is never checked. The check follows the role check, so a client that may not call cannot raise the token.
6. **A restore bumps the revision.** `restore.Apply` sets etcdutl's `RevisionBump` to 1,000,000,000 with `MarkCompacted`, the constant `revisionBump`, so a restored store continues above every revision the lost one reached after the snapshot and its leaders' tokens are not below one a host has seen.
7. **A 409 with that code ends the leader's term**, as a failed guard does.

## Consequences

- **A paused leader does nothing that lasts.** Its first etcd write after the pause fails the guard; its first call to a host that has seen the new leader is refused; both end its term.
- **Writes cost one nested transaction.** The guard adds a comparison to a transaction etcd already runs.
- **A host is fenced only after it has seen the new leader.** A stale leader's call to a host the new leader has not yet called goes through. The etcd guard bounds the damage to the host calls made before the stale leader's first write.
- **hostd keeps one more small file**, in its state directory.
- **A restored store skips a billion revisions.** Revisions are 64-bit and a cluster writes far fewer than a billion between snapshots; the cost is that a restored store's revisions are no longer dense, and etcd treats those before the bump as compacted.
- `lib/hostapi` gains a field in its error body and a client field, and `lib/wire` a constant; neither changes a call that carries no token.

## Signs the decision was wrong

- A host refuses a healthy leader after a restore, and the only way out is to delete the host's token file.
- A guarded write is found that has no term in its context, so it fails on every leader.
- A second writer to etcd is found among the leader's components that the guard does not cover.
- A cluster writes more than a billion revisions between two snapshots.
- The token has to be compared with something other than a revision, for instance across two etcd clusters.
