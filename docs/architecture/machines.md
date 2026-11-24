# Machines

What naru's Machine API is and how orch runs Machines across hosts: the API and its callers, the state orch keeps beside each host's, placement, moves, GPUs and the first use.

The API's types, client, conformance test and in-memory implementation are built (`lib/machineapi`), and so is the caller credential: registration with a one-time token and renewal, through the listener and the signer, and root's direct issuance; orch serves the API on its listener and keeps its records and each caller's event log ([orch.md](./orch.md) §The Machine API); its reconciler places each Machine on a host and runs it there, takes and deletes snapshots, and deletes Machines (§State, §Placement); exec, files and aliases answer 501, and a restore, placed and created from its snapshot's disks, is checked end to end with files (#383). The host API carries what a Machine needs on its host: its command, args, user, working directory, environment and files, its volumes as disks mounted at their paths, disk snapshots one per volume and a disk made from one, a stop's grace, and the reported labels ([host.md](./host.md)). Every other decision below is accepted and waits for the goal-3 issue that builds it. The PR that builds a part states that part here as built.

## Current decisions

### Layers

- **mvmd** runs Machines on one host behind its runtime API, which this does not change. **naru** runs them across hosts. **A use** (garam's Workspace first, a Function service later) calls naru's Machine API only, keeps its own resource and controller, and maps its names to Machines. The API names no use, and never a host ([ADR 0108](./adr/0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md) item 1).

### The API

- **A versioned public interface, `machines/v1alpha1`.**
  - **The specification** is this section, with the types and the Go client in `lib/machineapi`.
  - **The conformance test** is `lib/machineapi/machineapitest`, run through the client alone.
  - **The second implementation** is the in-memory `lib/machineapi/machineapifake`, while the version is below `v1`.
  - **Compatibility.** Additions (an operation, an optional field, a capability, a phase) keep the version. A removal, a rename or a change of meaning, idempotency or failure state is a new version. Alpha versions are served one at a time. A client takes an unknown phase as `unknown` and an unknown capability as absent.
  - **`v1`** needs a second production implementation ([ADR 0108](./adr/0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md) item 2).
- **Served by every orch member** over HTTPS with mutual TLS 1.3, on `--machine-api-addr` (port 7600, `lib/wire`), under `/machines/v1alpha1/`. That listener serves nothing else, and the operator API (port 7500) does not serve the Machine API.
- **Every member of a cluster is started with the same `--machine-zone`,** the zone of the stable names (`spec.orch.machineZone`). The first member to serve records it at `/naru/orch/machine-zone`. A member started with another zone answers every request 503 `unavailable`, so no Machine is named under a second zone. A member started with none serves no Machine API. Both log why.
- **Callers.** A caller is a use's provider at the place it runs: for garam, garam-bot's workspace provider. It holds a certificate of the role `caller`: OrganizationalUnit `caller`, CommonName the caller's name, a DNS label ([ADR 0116](./adr/0116-a-caller-registers-once-with-a-one-time-token-root-asks-the-signer-for-and-a-key-it-makes-where-it-runs-and-renews-its-own-certificate-superseding-adr-0108-item-4s-issuance.md)).
  - **Registration.** Root writes a one-time token for the name with `naructl caller token`; it lives an hour by default and a day at most. The caller makes its own key where it runs and registers once with the token at `POST /machines/v1alpha1/register` (no client certificate, limited per source and for all sources together), which orch passes to the signer's `POST /v1/caller-register`. A token is spent by its first presentation, whatever the outcome. The member logs a registration it limits, or the signer refuses, with the source and the nonce the token claims.
  - **Renewal.** In the last 30 days of its certificate the caller renews itself at `POST /machines/v1alpha1/renew`, signed with that certificate, for a new key; the certificate lasts 90 days, and the old one stays valid until it expires. A certificate renews once, so a caller that lost the answer registers again. A revoked or expired certificate renews nothing.
  - It is revocable like any pair, and root or an operator revokes every certificate of a caller by its name (`naructl caller revoke`). Root's direct issuance (`naructl caller credential`) is for tests and recovery. The signer's calls ([signer.md](./signer.md)), and the listener that admits a caller with its two routes ([orch.md](./orch.md) §The Machine API), are built. A caller reaches only its own Machines, snapshots and aliases; another caller's are 404. It reads no host, inventory or fleet event. Any other role is 403 on this listener.
- **Resources.**

  | Resource | Named by | Holds |
  | --- | --- | --- |
  | Machine | the caller (a DNS label), plus an `id` orch gives (16 random lowercase hexadecimal digits) that never changes | spec: image digest, `cpu`, `memoryBytes`, `rootDiskBytes`, `volumes` (`name`, `sizeBytes`, `mountPath`), `command`, `args`, `env`, `user`, `workingDir`, `files`, `services`, egress `networkPolicy`, `shape` (`region`, `labels`, `gpu`), `state` (`running` or `stopped`); status (below) |
  | Snapshot | the caller, unique within the caller | the Machine's volumes (mvmd disk snapshots) and its spec. No memory: it is consistent when taken stopped and crash-consistent when taken running. It outlives its Machine until the caller deletes it, and stays on the host it was taken on, where a restore is placed. |
  | Alias | its hostname | a use's own name for one of its Machines (ADR 0112) |

  The root disk is made from the image at each placement. What survives a move or a restore is on a volume.
- **Operations.**

  | Call | Meaning |
  | --- | --- |
  | `PUT .../machines/{name}` | Create. Idempotent by name, `state` left out of the comparison: 201 created, 200 the same spec, 409 another spec. With `fromSnapshot` it restores: the volumes are made from the snapshot, matched by name and size (422 otherwise). |
  | `GET .../machines`, `GET .../machines/{name}` | Spec and status. |
  | `POST .../machines/{name}/start`, `.../stop` | Desired state, idempotent. `stop` takes `graceSeconds`, at most 240, the most a host's stop takes (422 `invalid` above it); 0 is the host's 10. |
  | `DELETE .../machines/{name}` | 202. Its volumes go with it, and the name is held until they are gone. Its snapshots stay. |
  | `PUT .../snapshots/{snapshot}` (`machine`), `GET .../snapshots`, `GET`, `DELETE .../snapshots/{snapshot}` | Snapshots, idempotent by name; another Machine under the same name is 409. |
  | `POST .../machines/{name}/exec` | `command`, `stdin`, `timeoutSeconds`; returns the exit code, stdout and stderr. Running only. |
  | `GET`, `PUT .../machines/{name}/files?path=` | One guest file. Running only. |
  | `GET .../events?since=<cursor>` | Each status change of the caller's Machines after the cursor, from the caller's own log (20000 events, 7 days). A cursor older than the log is 410, and `GET .../machines` returns the cursor its listing is current at. |
  | `PUT`, `GET`, `DELETE .../aliases/{hostname}` | ADR 0112. |
  | `POST .../register` (`token`, `csr`) | The one operation with no client certificate. Spends a registration token for a caller certificate of the request's key (`certificate`, `notAfter`). A token not valid, spent or expired is 403 `token-refused`; more than 1 a second (burst 10) from one source (an IPv4 address or an IPv6 /64), or more than 1 a second (burst 30) from every source together, is 429 `rate-limited` ([ADR 0116](./adr/0116-a-caller-registers-once-with-a-one-time-token-root-asks-the-signer-for-and-a-key-it-makes-where-it-runs-and-renews-its-own-certificate-superseding-adr-0108-item-4s-issuance.md)). |
  | `POST .../renew` (`signed`) | A renewal the caller signed with its current certificate for a new key's request; answers like `register`. Before the certificate's last 30 days it is 409 `not-due`; a revoked or expired certificate, one of a caller revoked by name, or one that has renewed already is 403; a request for the current certificate's key is 400 `invalid`. |

  Exec and files are answered by the member that took the call, which calls the Machine's host. Errors are `{"error", "code"}`.
- **What `lib/machineapi` fixes beside the table.**
  - **Codes.** Each refusal has a code: `invalid` (400 for a body that is not the operation's JSON, 413 for one above the operation's limit, 422 for a field), `forbidden` (403), `not-found` and `file-not-found` (404), `conflict`, `deleting` (the held name) and `not-running` (409), `cursor-expired` (410), `machine-not-found`, `snapshot-not-found` and `snapshot-mismatch` (422), `alias-taken` (409), `alias-domain-not-granted` (403), `internal` (500), `unimplemented` (501) and `unavailable` (503). A client keeps a code it does not know.
  - **The rules every implementation applies** are functions of the library: the spec's checks (mvmd's for the run configuration, ADR 0111 item 6 for a GPU Machine), create's comparison, the zone's alias rules and an alias certificate's check.
  - **Secrets are never read back.** A read answers each env value as `<set>` and each file by path, mode and size, without its content, as mvmd's Inspect does; create compares the values it was given. So a spec read back and put again is 409: a use keeps its own spec.
  - **A request field the version does not name** is refused, 400 `invalid`, and not ignored.
  - **Events** are `status` (the Machine's status, which the event carries) or `deleted` (the Machine gone and its name free). A cursor the log never gave is 400.
  - **A snapshot** has a phase, `pending`, `ready` or `failed`, and a client reads an unknown one as `unknown`.
  - **A create's phase** is `pending`: the call records the Machine, and placement follows.
- **Status.**
  - `phase`: `pending`, `starting`, `running`, `stopping`, `stopped`, `moving`, `failed`, `deleting` or `unknown`.
  - `reason`: the part of the spec or shape that cannot be met, with no figure of the fleet's capacity; operators read the figures on the operator API.
  - `address`: the stable name, which no move changes. The private address is the host's and is on the operator API only.
  - `generation` and `observedGeneration`.
  - `capabilities`.
  - No host field. A host that does not answer shows as `unknown` with the reason "the Machine's host does not answer".
- **Capabilities** are values each Machine reports, in one API.
  - `snapshot`: `volumes` always. `memory` is not offered by `v1alpha1`'s implementation.
  - `move`: `cold` always; `snapshot` and `live` later.
  - `resize`: from the host Driver's resize capabilities.
  - `gpu`: the model and count attached.

  A call a Machine's capabilities exclude is 409 naming the capability ([ADR 0108](./adr/0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md) item 8; [ADR 0111](./adr/0111-a-gpu-machine-runs-on-the-cloud-hypervisor-driver-with-whole-devices-by-vfio-on-a-bare-metal-host-pinned-to-the-devices-numa-node-with-hugepages.md) item 7).

### State

- **Each host is authoritative for what runs on it and keeps running it without orch:** its mvmd's record and hostd's report. Orch takes a Machine's status from its bound host's last report and infers nothing about a host that does not answer. hostd renews mvmd's host-wide deadline itself, so a lost orch, its quorum included, stops no Machine; every Machine is created with `restartOnBoot`, fixed at create, so one that was running comes back after its host reboots without orch, and one stopped through the API stays stopped ([ADR 0109](./adr/0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md) item 1).
- **Orch keeps, in etcd:** the Machine record (`/naru/orch/machines/<caller>/<name>`: spec, desired state, `id`, `generation`), written by the API; the binding (host, epoch), the move record and a host's abandonment record, written by the leader through `leader.Guard`; the routes derived from them. Every rule deciding whether a Machine may run elsewhere reads only these and the hosts' answers.
- **On a host, a Machine is the microVM `m-<id>-<epoch>`.** The name carries no caller; the epoch counts placements. The prefix `m-` is reserved for Machines: no group name begins with it ([groups.md](./groups.md)), so no group member is named as a Machine's microVM.
- **The reconciler** `orch/internal/machines` runs on the leader, one pass per `--machine-interval` (default 5 s) and at once after a Machine API write by any member. Built:
  - **Waking.** An etcd watch wakes it on what only the API writes: a Machine created, given a new generation or marked deleting, and a snapshot created or deleted. Its own writes (a phase, a binding, a snapshot's host) wake no pass. A watch that closes is opened again after a backoff from 1 s to 30 s, with a log line, and a pass follows.
  - **One reading.** It reads the hosts, the Machines, the snapshots, the allocations, the deletions and the tombstones once, and plans from that reading alone.
  - **Work per host.** What needs no host is done first. Each answering host then gets its own work, bounded at 10 minutes, and the pass waits for it at most one interval. A host whose work outlasts that keeps it, and the passes that follow give it none until it ends, nor does a pass that began to read while it ran, since what that pass read of the host may be older than that work's writes; so one slow host delays only its own Machines. Each host's work reads the Machines as they were when the work began, and writes only its own.
  - **Creating.** On the bound host it creates the microVM `m-<id>-<epoch>`, unfenced, with `restartOnBoot`, the run configuration, and each volume as a disk at its path.
    - It writes `starting`, `stopping` and the phase the host reports (`running`, `stopped`, or `failed` for a faulted microVM), and `observedGeneration` once it acted on a generation. A start or stop shows `starting` or `stopping` at once on a placed Machine.
    - The host API starts every microVM it creates, so a Machine created or restored with `state: stopped` runs until the same pass stops it, and shows `stopping` meanwhile. A create that does not start is #434's.
    - No create is made from a host's answer that its probe repeated, since a microVM made since would be missing from it.
  - **A refused create.** A create the host refuses (a 4xx other than 401, 403, 404, 409 and 429) fails the Machine with the host's message, and is not made again for that generation; a start or stop, a new generation, is created again. Any other failure is retried on the next pass.
  - **Restarts.** A desired state it has not yet acted on is acted on at once. A Machine that stopped on its own after that, while desired running, is started again at most once a minute; each attempt is a `machine.restart` fleet event, and between attempts the Machine shows `stopped` or `failed` with that reason.
  - **Stopping.** A stop gives the host the grace the caller asked for, at most 240 s; a grace stored before the bound is given as 240.
  - **A host that does not answer** leaves its Machines `unknown` with the reason "the Machine's host does not answer" (the operator reads the host too), and nothing of them is placed elsewhere. A deleted Machine on it stays `deleting`, its name held.
  - **Deleting.** A deleted Machine's microVM is deleted with its disks, then the record goes, freeing the name and the id and giving its allocation back, with the `deleted` event and a tombstone at `/naru/orch/machine-tombstones/<id>`. One deleted before it was placed goes at once.
  - **What no record names.** On each host that answers, the reconciler reaches only a microVM named `m-<16 hex digits>-<epoch>`; any other name is outside its reach.
    - One whose id a tombstone names, such as one a late create left, is deleted with its disks.
    - One whose id has neither a record nor a tombstone, as after an etcd restore, is left as it is, disks and all, with one `machine.unknown-microvm` fleet event: a missing record is no deleted Machine.
    - One of a live Machine that is not its binding (another host or epoch) is stopped and left with its disks for the move that settles it (#384), with a `machine.older-epoch` fleet event.
    - A placement takes the epoch above any a host reports for the id, so it never reuses an earlier placement's name.
    - A tombstone is kept 7 days: a microVM a late create left is found on the first pass its host answers, and one whose host stays away longer is left as unknown.
  - **Snapshots.**
    - A pending snapshot is taken on its Machine's host: the snapshot records the host first, then the host makes one disk snapshot per volume (`s-<snapshot id>-<i>`), then the snapshot is `ready`.
    - One whose Machine is gone or being deleted fails. One the host refuses three times (the same classification as a create) fails with the host's message, and what it took is deleted.
    - A deleted or failed snapshot taken on a host leaves `/naru/orch/snapshot-deletions/<id>`. Once the host has deleted its disk snapshots it is marked done and kept 7 days as the snapshot's tombstone. A disk snapshot named `s-<16 hex digits>-<i>` whose id a done deletion names is deleted; one whose id has neither a snapshot nor a deletion is left, with one `machine.unknown-disk-snapshot` fleet event.
  - **The allocation's audit.** Each pass compares each host's allocation with the sum of its bindings, and logs a mismatch at error level, with a `machine.allocation-drift` fleet event once per change. Placement still uses the allocation, which the guards compare.
  - Machines are not fenced per microVM.
- **A Machine is rebound only on its old host's acknowledgement, or behind an operator's fence of the host** ([ADR 0109](./adr/0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md) items 5 and 6). A host that does not answer keeps its Machines `unknown`, with their volumes. A host cut off from orch but alive keeps renewing its own deadline and never fetches the revocation list, so the operator first fences it outside naru (powers it off through its hypervisor or BMC, or confirms it is off) and then runs `naructl host abandon <id>` with `--fenced "<how>"`, which refuses a request without the attestation: the abandonment and the attestation (who, when, how) are recorded, the host's certificate revoked, and the host taken out of every peer list and route; its Machines become `lost`, and each is deleted by its use or moved by an operator with `--abandon-volumes` (new, empty volumes; an event names those left behind). The attestation commits the operator to bringing the host back only through a reset of its data or a reinstall: a fenced host powered on as it was restarts every Machine it ran (all have `restartOnBoot`), with live egress, until it reaches orch and for up to 10 minutes after, or without limit while partitioned. If it does reach orch it finds its pair revoked; hostd checks its pair before its first renewal at start, exits, and the host's Machines stop when the host-wide deadline lapses, within 10 minutes, unreachable meanwhile. naru fencing through hypervisors and BMCs itself is the later goal's.
- **A disk is deleted only when a verified copy exists elsewhere or its Machine is deleted** ([ADR 0109](./adr/0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md) item 7). The volumes of an abandoned epoch are kept and listed (`GET /v1/machines/abandoned-volumes`) until an operator deletes them (`naructl machine abandoned-volumes delete`).
- **The first goal runs one orch member** on its two hosts; three come with the third host ([ADR 0109](./adr/0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md) item 9).

### Moves

- **A move is recorded step by step** so a new leader continues it ([ADR 0109](./adr/0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md) item 5):
  1. reserve the target;
  2. stop on the source, and wait for the source to report it stopped (the acknowledgement);
  3. copy each volume host to host over the mesh, the target pulling with a grant bound to its host ID and both sides comparing the SHA-256;
  4. create the microVM on the target under the next epoch, not started;
  5. commit: rebind and move the routes in one transaction;
  6. start on the target;
  7. clean up the source's microVM and volumes once the target has run and every proxy has the committed routes.

  An abort before the commit deletes what the target holds and starts the source; the target has never run, so no second copy exists. After the commit the move completes forward. The root disk is remade from the image; snapshots stay where they were taken.
- **The stream is mvmd's.** mvmd gains a stream read of a disk no running Machine holds (or of a disk snapshot) and a Create disk from a stream with size and SHA-256 (garamsh/mvmd#827). The host API gains `POST /v1/disks/{name}/export`, which answers a grant, and `POST /v1/disks/import`. The export is served once, within 10 minutes, to the host certificate the grant names.
- **Moves happen only when asked:** an operator's `POST /v1/machines/{caller}/{name}/move` (`naructl machine move`), or a drain, one Machine at a time per host and two in the fleet. A Machine that fits nowhere else holds the drain, with an event ([ADR 0110](./adr/0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md) item 7).

### Placement

- **Orch keeps its own account** ([ADR 0110](./adr/0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md)). A host's reported capacity, less the host reserve (`--host-reserve-cpu` 1, `--host-reserve-memory` 2 GiB), less every Machine bound there (running or stopped, volumes included), every reservation, every snapshot kept there (its disk snapshots' allocated bytes), and every other microVM it reports, a group's member included. Nothing is overcommitted in `v1alpha1`. A binding commits in one guarded transaction with `/naru/orch/host-allocations/<host>`, only while that record is the one the plan read; the groups engine commits its members through the same record while groups exist. Built: the record holds the sum of what the host's bindings take, a Machine's removal gives its part back in the transaction that removes it, and the reservations come with moves (#384). The groups engine does not commit through it yet: its members are counted only once a host reports them, so a member and a Machine placed before that report can take the same room ([#415](https://github.com/garamsh/naru/issues/415)).
- **Labels.** The host reports `naru.garam.sh/virtualization` (`metal`, or `nested` under the `hypervisor` CPU flag) and its GPUs. The operator sets `naru.garam.sh/site` and the rest. The two sets are separate fields. Built on the host: `GET /v1/host` answers `reportedLabels` apart from `labels`, and `spec.host.labels` refuses a reported key ([host.md](./host.md)); the GPUs are the later goal's.
- **Filter, then spread.** A candidate is admitted, answering and not draining, matches the shape's region and labels, and has the GPUs, CPU, memory and disk free. The winner has the largest share of allocatable memory left, with ties broken by rendezvous hashing of the Machine's `id`. A GPU host takes a Machine without GPUs only when nothing else fits. A restore is placed on the snapshot's host or waits. Built in that order, a label matching the operator's set or the host's reported one; no host reports GPUs before the later goal, so a GPU Machine waits, and the rule for GPU hosts applies from then. A restore waits `pending` until its snapshot is `ready`; its reason names the snapshot's host ("the snapshot's host does not meet it: …").
- **No fit waits.** The phase is `pending`, the reason names the first condition no host met (the caller sees which, the operator also the figures), and each pass retries. Built: the operator API's `GET /v1/machines` carries the figures as `reason`, such as "no host has 4.0 GiB of memory free; the most free is 2.5 GiB", counted over the hosts that met the conditions before it.

### GPUs (later goal)

- **The Driver** is mvmd's Cloud Hypervisor Driver with whole devices by VFIO, one Machine per device, on bare-metal hosts only ([ADR 0111](./adr/0111-a-gpu-machine-runs-on-the-cloud-hypervisor-driver-with-whole-devices-by-vfio-on-a-bare-metal-host-pinned-to-the-devices-numa-node-with-hugepages.md)).
- **The host.** It binds the devices of `spec.host.gpus` to `vfio-pci` at boot, with the IOMMU on, and reserves 1 GiB hugepages per NUMA node from `spec.host.hugepages`. It reports each device's model, NUMA node and IOMMU group. A device whose group holds anything but bridges and its own functions is not offered.
- **The request.** A use asks for `gpu: {model, count}`. Orch places by free count, and mvmd chooses the devices on as few NUMA nodes as hold them.
- **Performance.** The spec's `cpu` is at least 8 per GPU and its `memoryBytes` a whole number of GiB (422 otherwise); the vCPUs are pinned one to one to free cores of those nodes, and the memory is 1 GiB hugepages there.
- **What it can do.** `snapshot: [volumes]`, `move: [cold]`, no `resize`.
- **The guest kernel variant** carries the NVIDIA open modules. The image carries CUDA.

### The first user

garam's Workspace is the first caller. This is the interface between the two; garam's controller is garam's to design.

- **The credential.** garam holds none. garam-bot's workspace provider, at the execution place (the cluster connector or the naru-host connector), is the caller: it registers once with a token root gave its install, keeps its key where it runs, and renews its certificate itself. garam declares the Workspaces it drives.
- **One Workspace is one Machine.** garam names it, sets its image, resources, one volume for the user's files and a `services` entry for its HTTP port, and publishes the Workspace's URL as an alias of the Machine (ADR 0112).
- **The calls.**
  - Start and stop through the API.
  - Snapshot the volume through the API.
  - Follow `GET .../events` for status, not polling.
  - Delete the Machine with the Workspace.
- **What garam never sees:** a host, a host address or a move. A move shows only as `phase: moving` and back.

## Rationale

- [ADR 0108](./adr/0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md): why a versioned interface with a conformance test and a fake; why orch, on a listener of its own, and not a unit, the operator API, gRPC, the proxy or Kubernetes resources; why a certificate role and not tokens or `automation`; why snapshots hold volumes and not memory.
- [ADR 0116](./adr/0116-a-caller-registers-once-with-a-one-time-token-root-asks-the-signer-for-and-a-key-it-makes-where-it-runs-and-renews-its-own-certificate-superseding-adr-0108-item-4s-issuance.md): why the caller is the provider at the place and not garam; why a token the operator authority signs and the revocation list spends, and not one the signer stores; why renewal only in the last 30 days, without revoking the old pair, and once per certificate; why a caller is revoked by name through the list.
- [ADR 0109](./adr/0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md): why the host is the truth and keeps running without orch (flyd, E2B, OpenNebula weighed); why etcd and not gossip; why no per-microVM fence, the target started only after the commit, and an abandonment for a lost host; why stop, copy and start, host to host, without the root disk; why one orch member on two hosts.
- [ADR 0110](./adr/0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md): why orch's own account and not `free`; why spread; why a pending Machine waits; why no rebalancing; why not the groups engine.
- [ADR 0111](./adr/0111-a-gpu-machine-runs-on-the-cloud-hypervisor-driver-with-whole-devices-by-vfio-on-a-bare-metal-host-pinned-to-the-devices-numa-node-with-hugepages.md): why Cloud Hypervisor and not QEMU (with Ubicloud's B200 finding weighed); why bare metal; why mvmd chooses the devices; why pinning and hugepages; how the goal measures it.

## Open questions

- **Changing a Machine's spec.** `v1alpha1` refuses another spec under the same name (409), so a resize is a delete and a create by the use. An update operation is an addition.
- **CPU overcommit.** Nothing is overcommitted. A ratio waits for a measurement of idle workspaces.
- **Machine-to-Machine traffic.** A Machine is reached only through the proxy at its services. Private traffic between a caller's Machines is not decided.
- **Quotas per caller.** There is one caller. A quota is decided when a second caller needs to be held apart.
- **A memory snapshot, and the snapshot and live moves.** They need mvmd to restore onto new disk identifiers, and the same Driver, version and CPU model on the target. They are the later goal's.
- **Exec streams.** Exec returns its output whole. A streaming exec, perhaps over gRPC, is an addition.
