# ADR 0053: a member started with --restore serves only a snapshot upload and the operator sends it the plain snapshot, and the other members join it as new members

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#118 (garamsh/aurora#811 A5, orch half; plan: garamsh/naru#113). It extends [ADR 0041](./0041-orch-is-a-cluster-of-members-with-an-embedded-etcd-one-elected-leader-and-an-mtls-api-that-enrolls-and-admits-hosts-and-the-signer-holds-every-key.md), whose snapshots were written sealed and could only be opened by hand.

## Context

- **Snapshots could be written and not used.** Every 6 hours the leader's member writes a snapshot sealed to the recovery key, and `naructl orch restore` could open one into a plain file. Nothing put it into a new member, so a control plane lost with all its members could not be brought back by the product.
- **A new member has no etcd to ask.** The restore has to happen before etcd starts, and the member's mutual-TLS API is the only door the operator already has, so the question is which door a member without etcd opens.
- **etcd restores from a snapshot only into a new cluster.** A restored data directory is a one-member cluster with a new cluster ID; the other members cannot continue their old data against it.
- **The plain snapshot holds every secret orch stores.** It must not be written where it need not be.

## Decision

1. **`naru-orchd --restore` starts no etcd when the data directory holds no member.** It serves the API's mutual TLS listener with one route, `PUT /v1/restore`, root only; every other route is 503, "this member waits for a restore". With a member in the data directory, `--restore` is ignored and logged, so the flag can stay in a service's arguments after the restore.
2. **The body is the plain etcd snapshot, with its SHA-256 in `X-Naru-Snapshot-Sha256`.** Orch writes the body to a temporary file beside the data directory and refuses with 400 if its SHA-256 is not the header's (the whole 64 hex digits) or if etcd's own snapshot check, in `etcdutl`'s restore, refuses it. It restores into a staging directory with this member's name, its `--peer-url` and an initial cluster of itself alone, renames the staging directory to the data directory, deletes the temporary file and starts etcd and the rest of the member in the same process. The listener does not restart: the handler is replaced by the full API. A second `PUT /v1/restore` is 409 and changes nothing.
3. **The other members join as new members.** The first member restores with an initial cluster of itself alone. A second and third start with `--join <a running member's client URL>` and `--initial-cluster-state existing`: before starting etcd, the member calls `MemberList` and `MemberAdd` with its own peer URL, then starts with the initial cluster etcd returns. `MemberAdd` is retried every second, to a minute, while etcd answers that the cluster is unhealthy or that not enough members have started; a member added a moment ago has not been connected for the 5 seconds etcd wants, and the third add is refused until it has.
4. **`GET /v1/snapshots/latest` returns the newest sealed snapshot's bytes, root only.** The guest directory is `/var/lib/naru/orch/snapshots`; with no snapshot the answer is 404. An operator takes one at once with `naructl orch backup --out <file>`, which calls `POST /v1/snapshot` and then fetches it, and the sealed file is opened only with the recovery key.
5. **`naructl orch restore --push <orch URL>` opens the sealed snapshot on the operator's machine and sends the plain bytes.** It takes `--snapshot`, `--recovery-key` and `--passphrase-file` as before, and `--orch-ca`, `--admin` (root's person file) and `--admin-passphrase-file` (the `--passphrase-file` when omitted) for the connection. The plain snapshot is written to disk on the operator's machine only when `--out` is also given. `--out` or `--push` is required.
6. **The orch API counts its requests** in `naru_orch_api_requests_total{route,code}`, labelled by the registered route pattern, never the path, and registered with the registry `--metrics-addr` serves.

## Rejected

- **Restoring with `etcdutl` by hand on the guest.** The guest has no shell an operator is meant to use, and the product's own path is what the plan promises.
- **A restore that every member does from a sealed snapshot with the recovery key on the guest.** The recovery key's passphrase would live on a control host.
- **Restoring all three members with the same snapshot and an initial cluster of three.** It works, and needs the operator to reach three members before any serves; a first member alone is serving at once and the others catch up from it.
- **Starting etcd empty and replaying the snapshot through the API.** It does not carry revisions, leases or the cluster identity, and the file already is etcd's format.
- **Restarting the listener after the restore.** A window with no listener makes the operator's call fail after it succeeded.

## Accepted risks

- **The plain snapshot is on the control host's disk, in the data directory's parent, until the restore ends.** It is removed on success and failure; a crash in between leaves it there.
- **Every `restore.Apply` failure is a 400**, a full disk included.
- **The member that restores trusts whoever holds root's certificate to send the right snapshot.** The hash checks the transfer, not the choice of snapshot.

## Consequences

- A lost control plane is brought back from a sealed snapshot with the recovery key, root's person file and `naructl`, on members the image already boots.
- `lib/orchapi` gains `Client.Restore`, `Client.LatestSnapshot` and the hash header; `orch` gains `internal/restore`, `--restore` and `--join`; `naructl orch backup` gains `--out`, and `orch restore` gains `--push`.
- A restored cluster has a new etcd cluster ID: hosts keep their certificates and reconnect, and orch's other state is what the snapshot held.

## Signs the decision was wrong

- Operators restore by `etcdutl` anyway, because `--push` is out of reach on a lost control plane: the door has to be somewhere else.
- A third member's `MemberAdd` still fails after the retry: the join needs a wait on etcd's health that this does not give.
- A crash during a restore leaves a plain snapshot on disk often enough to matter: the file should live in memory or be written under a stricter directory.
