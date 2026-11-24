# Machines

What naru's Machine API is and how orch runs Machines across hosts: the API and its callers, the state orch keeps beside each host's, placement, moves, GPUs and the first use.

None of it is built yet. Each decision below is accepted and waits for the goal-3 issue that builds it. The PR that builds a part states that part here as built.

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
- **Callers.** A caller holds a certificate of the role `caller`: OrganizationalUnit `caller`, CommonName the caller's name, a DNS label. Root issues it at the signer's `POST /v1/caller`, through `naructl caller credential` with `--name`, for 90 days at most. It is revocable like any pair. A caller reaches only its own Machines, snapshots and aliases; another caller's are 404. It reads no host, inventory or fleet event. Any other role is 403 on this listener.
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
  | `POST .../machines/{name}/start`, `.../stop` | Desired state, idempotent. `stop` takes `graceSeconds`. |
  | `DELETE .../machines/{name}` | 202. Its volumes go with it, and the name is held until they are gone. Its snapshots stay. |
  | `PUT .../snapshots/{snapshot}` (`machine`), `GET .../snapshots`, `GET`, `DELETE .../snapshots/{snapshot}` | Snapshots, idempotent by name; another Machine under the same name is 409. |
  | `POST .../machines/{name}/exec` | `command`, `stdin`, `timeoutSeconds`; returns the exit code, stdout and stderr. Running only. |
  | `GET`, `PUT .../machines/{name}/files?path=` | One guest file. Running only. |
  | `GET .../events?since=<cursor>` | Each status change of the caller's Machines after the cursor, from the caller's own log (20000 events, 7 days). A cursor older than the log is 410, and `GET .../machines` returns the cursor its listing is current at. |
  | `PUT`, `GET`, `DELETE .../aliases/{hostname}` | ADR 0112. |

  Exec and files are answered by the member that took the call, which calls the Machine's host. Errors are `{"error", "code"}`.
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
- **On a host, a Machine is the microVM `m-<id>-<epoch>`.** The name carries no caller; the epoch counts placements.
- **The reconciler** `orch/internal/machines` runs on the leader, one pass per `--machine-interval` (default 5 s) and at once after an API write. It creates each bound microVM with the volumes as disks, starts and stops it to the desired state, and deletes it after the Machine. A Machine that stopped on its own while desired running is restarted at most once a minute. A microVM without the `m-` prefix is outside its reach. Machines are not fenced per microVM.
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

- **Orch keeps its own account** ([ADR 0110](./adr/0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md)). A host's reported capacity, less the host reserve (`--host-reserve-cpu` 1, `--host-reserve-memory` 2 GiB), less every Machine bound there (running or stopped, volumes included), every reservation, every snapshot kept there, and every other microVM it reports, a group's member included. Nothing is overcommitted in `v1alpha1`. A binding commits in one guarded transaction with `/naru/orch/host-allocations/<host>`, only while that record is the one the plan read; the groups engine commits its members through the same record while groups exist.
- **Labels.** The host reports `naru.garam.sh/virtualization` (`metal`, or `nested` under the `hypervisor` CPU flag) and its GPUs. The operator sets `naru.garam.sh/site` and the rest. The two sets are separate fields.
- **Filter, then spread.** A candidate is admitted, answering and not draining, matches the shape's region and labels, and has the GPUs, CPU, memory and disk free. The winner has the largest share of allocatable memory left, with ties broken by rendezvous hashing of the Machine's `id`. A GPU host takes a Machine without GPUs only when nothing else fits. A restore is placed on the snapshot's host or waits.
- **No fit waits.** The phase is `pending`, the reason names the first condition no host met (the caller sees which, the operator also the figures), and each pass retries.

### GPUs (later goal)

- **The Driver** is mvmd's Cloud Hypervisor Driver with whole devices by VFIO, one Machine per device, on bare-metal hosts only ([ADR 0111](./adr/0111-a-gpu-machine-runs-on-the-cloud-hypervisor-driver-with-whole-devices-by-vfio-on-a-bare-metal-host-pinned-to-the-devices-numa-node-with-hugepages.md)).
- **The host.** It binds the devices of `spec.host.gpus` to `vfio-pci` at boot, with the IOMMU on, and reserves 1 GiB hugepages per NUMA node from `spec.host.hugepages`. It reports each device's model, NUMA node and IOMMU group. A device whose group holds anything but bridges and its own functions is not offered.
- **The request.** A use asks for `gpu: {model, count}`. Orch places by free count, and mvmd chooses the devices on as few NUMA nodes as hold them.
- **Performance.** The spec's `cpu` is at least 8 per GPU and its `memoryBytes` a whole number of GiB (422 otherwise); the vCPUs are pinned one to one to free cores of those nodes, and the memory is 1 GiB hugepages there.
- **What it can do.** `snapshot: [volumes]`, `move: [cold]`, no `resize`.
- **The guest kernel variant** carries the NVIDIA open modules. The image carries CUDA.

### The first user

garam's Workspace is the first caller. This is the interface between the two; garam's controller is garam's to design.

- **The credential.** garam holds a `caller` pair named `garam` in AWS Secrets Manager and renews it before its 90 days run out.
- **One Workspace is one Machine.** garam names it, sets its image, resources, one volume for the user's files and a `services` entry for its HTTP port, and publishes the Workspace's URL as an alias of the Machine (ADR 0112).
- **The calls.**
  - Start and stop through the API.
  - Snapshot the volume through the API.
  - Follow `GET .../events` for status, not polling.
  - Delete the Machine with the Workspace.
- **What garam never sees:** a host, a host address or a move. A move shows only as `phase: moving` and back.

## Rationale

- [ADR 0108](./adr/0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md): why a versioned interface with a conformance test and a fake; why orch, on a listener of its own, and not a unit, the operator API, gRPC, the proxy or Kubernetes resources; why a certificate role and not tokens or `automation`; why snapshots hold volumes and not memory.
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
