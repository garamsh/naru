# ADR 0043: the host API snapshots and resumes a microVM, gives it a fence of its own, and its types and client are the module lib/hostapi

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#120 (plan: garamsh/naru#113). It extends [ADR 0028](./0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md), [ADR 0035](./0035-the-host-api-creates-a-microvm-of-an-os-form-from-a-boot-set-with-a-data-disk-and-a-nocloud-seed-and-the-iso-9660-writer-is-a-library-module.md) and [ADR 0038](./0038-the-host-api-stops-and-starts-a-microvm-and-restart-on-boot-brings-a-microvm-back-after-its-host-restarts-and-naructl-takes-the-os-forms-flags.md) by reference: their bodies are not edited, and what they say holds with this decision added.

## Context

- **A control plane above the host must keep a microVM's state and survive losing it.** mvmd at b1a60cd has a snapshot of a Running or Paused Machine, a restore of a snapshot into a new Machine, and a fence per Machine (mvmd ADR 0064) next to the node-wide fencing deadline hostd renews.
- **mvmd's restore creates a new Machine.** It does not resume the Machine the snapshot was taken of. The new Machine takes the snapshot's disks, which the node must hold unattached and unchanged since the snapshot, and takes its network policy and routed prefixes from the request, since a snapshot carries neither. mvmd has no inspect call for a snapshot, only a list ordered by id. It refuses the snapshot of an OS-form Machine.
- **A fenced Machine is not armed** at creation, after a lapse or after a host's boot, and mvmd refuses to start it until its fence is renewed. A lapse ends its VMM and sets `fence_lapsed_at` and a `last_failure_reason` that begins "the Machine's own fence lapsed".
- **orch calls the host API from Go.** naructl has its own client and types for it, and a second copy in orch would be a third.

## Decision

1. **The snapshot.**
   - `POST /v1/microvms/{name}/snapshot` takes `{"name": "<snapshot>"}`, a DNS label of the caller's choosing, and answers 201 with the snapshot. The same name again is the snapshot already taken. A microVM mvmd cannot snapshot (not running, an OS form, a driver that cannot) is 409 with mvmd's reason, and the microVM is left as it was.
   - `GET /v1/snapshots` and `GET /v1/snapshots/{name}` show the snapshots (`name`, `microvm`, `imageDigest`, `driver`, `cpuModel`, `kernel`, `rootDiskBytes`, `disks`, `sizeBytes`, `createdAt`), ordered by name. `DELETE /v1/snapshots/{name}` deletes one and answers 204, also for an absent one; one in use is 409.
2. **Resume is create-from-snapshot,** because that is mvmd's shape. `POST /v1/snapshots/{name}/resume` takes `{name, networkPolicy, podPrefix}` and answers 201 with a new running microVM of that name. A snapshot that mvmd cannot restore on this host (its driver, version or CPU differs, a disk is held or has changed) is 409 with mvmd's reason. This is not the `POST /v1/microvms/{id}/resume` the issue sketches: that path would imply the source microVM is the one resumed.
3. **The fence of one microVM.**
   - `fenced: true` on `POST /v1/microvms` passes to mvmd's `fenced`, and the microVM shows `fenced`. A fenced create answers 201 with the microVM Created, not started, and so does an identical repeat; a different `fenced` on a repeated request is 409.
   - `POST /v1/microvms/{name}/fence` takes `{lengthSeconds, renewal}` and answers 200 with the fence (`phase`, `lengthSeconds`, `remainingSeconds`, `renewal`); `GET` on the path shows it. `phase` is `notArmed`, `armed` or `expired`. The length is 1 to 86400 seconds and the renewal number is above 0, else 422; a renewal number not above the last applied changes nothing. A microVM that is not fenced is 409, an unknown one 404.
   - The caller renews, then starts: `POST .../start` of a fenced microVM that is not armed is 409, mvmd's refusal.
   - The microVM shows `fenceLapsedAt` when its fence has lapsed, and `failure` carries mvmd's reason.
   - hostd renews no microVM's fence. The caller does, and the node-wide deadline hostd renews is unchanged.
4. **Roles.** The new GETs are for a reader and a provisioner. The snapshot, the resume, the delete of a snapshot and the renewal are for a provisioner only.
5. **`lib/hostapi`** is the module `github.com/garamsh/naru/lib/hostapi`: the request and response types of the host API, the policy types they hold, and a client (`NewClient(base, *http.Client)`, a method per endpoint, a refusal as `*hostapi.Error`). hostd's server and orch use the same types, and mvmd's package in hostd aliases the policy and kernel types. naructl keeps its own client until it is moved.
6. **The mvmd pin** of hostd's `lib/runtimeapi` moves to b1a60cd649673a6db5fa72ca8cc23f088fd83639. The pin of the mvmd the image builds, `MVMD_COMMIT`, is not moved here; it moves at the end of the round, and a host built before then has an mvmd without these calls.

## Consequences

- A microVM with named disks cannot be resumed after a hostd delete of it: hostd deletes the disks `<name>.*`, and mvmd needs the snapshot's disks. A snapshot of an image-form microVM with only its root disk is resumable after the delete.
- mvmd has no inspect call for a snapshot, so a snapshot's get lists them all; the cost is the number of snapshots on a host.
- A lapse leaves the microVM Stopped with its failure; the caller renews the fence and starts it again, as after any stop.
- orch and hostd cannot disagree on a field's name, since both compile the same type.

## Signs the decision was wrong

- A caller needs the source microVM itself back, not a new one: mvmd gains a resume of the same Machine and the path the issue sketched is right.
- Snapshots on a host are numerous enough that listing them to inspect one is slow: mvmd gets an inspect call and hostd uses it.
- Callers keep lapsing fences because they renew late: hostd renews the fence of a microVM it was told to keep.
- A snapshot of a microVM with named disks is wanted after its delete: delete keeps the disks the snapshot records.
