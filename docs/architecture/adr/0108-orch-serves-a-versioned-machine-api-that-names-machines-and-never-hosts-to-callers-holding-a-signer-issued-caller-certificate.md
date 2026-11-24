# ADR 0108: orch serves a versioned Machine API that names Machines and never hosts, to callers holding a signer-issued caller certificate

> Status: accepted; item 4's issuance (who the caller is and how it gets its certificate) superseded by [ADR 0116](./0116-a-caller-registers-once-with-a-one-time-token-root-asks-the-signer-for-and-a-key-it-makes-where-it-runs-and-renews-its-own-certificate-superseding-adr-0108-item-4s-issuance.md)
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#373 (goal 3, "naru: Machines across hosts"). It is the first of four decisions on Machines across hosts: this one is the API; [ADR 0109](./0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md) is the state and the moves, [ADR 0110](./0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md) the placement and [ADR 0111](./0111-a-gpu-machine-runs-on-the-cloud-hypervisor-driver-with-whole-devices-by-vfio-on-a-bare-metal-host-pinned-to-the-devices-numa-node-with-hugepages.md) the GPU. Routing to a Machine and the host mesh are ADRs 0112 and 0113.

## Context

- **The direction.** #373 §2 and its amendment of 2026-11-24 make naru the multi-host microVM service. Its core resource is the Machine, the word mvmd uses and Fly.io's Machines use. Uses sit above naru and call its API only: garam's Workspace first, a Function service later. The API knows no workspace, function, user or organisation, and it never names a host.
- **What exists.** mvmd runs Machines on one host behind its runtime API `v1alpha2` (garamsh/mvmd `docs/architecture/runtime-api.md` at `2a07ba313c0a2c424ba9badf40ea9c36ec670e14`). naru-hostd wraps it as the host API for orch ([host.md](../host.md)). Orch keeps the fleet in an embedded etcd and serves an operator API on port 7500 ([orch.md](../orch.md)). Nothing serves a caller that is neither an operator nor a host.
- **The interface rule.** garamsh/convention-driven-project `docs/convention/interfaces.md` (merged by PR #594, file at `aea0a7d222f693855f47188df5e11e47f2af5d62`) binds a boundary between separately deployed components: "The boundary is a versioned public interface", "Each interface has a conformance test", "More than one implementation from the start" (a reference implementation counts as the second in pre-stable releases), and "The specification states its compatibility rules". garam is deployed separately from naru, so this boundary is one.
- **Precedents.** Fly.io's Machines API addresses a Machine by app and ID, `/v1/apps/{app_name}/machines/{machine_id}`, and places it by region, never by host (docs.fly.io/machines/api/machines-resource, read 2026-11-24). E2B's API "decides _where_ a sandbox runs and records _that_ it runs", and "Sandbox traffic goes straight from the edge to the node and never passes through the API" (e2b-dev/runtime `README.md` at `30a39cf3364197c162576f3823bb7e927f6c82d5`). OpenNebula's front-end takes a VM template whose `SCHED_REQUIREMENTS` filter hosts by attribute (docs.opennebula.io/7.4, scheduler overview), so a template names a shape, not a host.

## Decision

1. **Three layers, each calling only the one below.** mvmd runs Machines on one host, and its runtime API does not change for this. naru runs Machines across hosts: placement, the per-host state it reconciles from, moves, and routing to a Machine's stable name. A use holds its own resource and controller, maps its names to Machines and calls naru's Machine API alone. The API's types name nothing of any use.
2. **The Machine API is a versioned public interface, `machines/v1alpha1`.**
   - **The specification** is [machines.md](../machines.md) §The API, with the request and response types and the Go client in a new library module, `lib/machineapi`.
   - **The conformance test** is the package `lib/machineapi/machineapitest`. It drives an implementation through `machineapi.Client` alone and reaches into no implementation.
   - **The second implementation** is `lib/machineapi/machineapifake`, an in-memory implementation that passes the conformance test. It counts as the second while the version is alpha or beta, as interfaces.md allows. Orch is the first. A `v1` needs a second production implementation, so the API stays below `v1` until one exists.
   - **Compatibility.** Adding an operation, an optional field, a capability or a phase keeps the version. Removing or renaming anything, or changing what an operation means, its idempotency or its state after failure, is a new version. That is mvmd's rule for its runtime API, applied to this one. Alpha versions are served one at a time. A deprecated element is retired only by a new version. A client therefore treats an unknown phase as `unknown` and an unknown capability as absent.
3. **Orch serves it, on a listener of its own.** Every orch member serves the Machine API over HTTPS with mutual TLS 1.3, on `--machine-api-addr` (port 7600, a constant of `lib/wire`), under the path prefix `/machines/v1alpha1/`. Any member answers. A write goes to etcd, and the leader's reconciler acts on it ([ADR 0109](./0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md)). The operator API on port 7500 does not serve the Machine API, and the Machine API's listener serves no other route. So the caller's network reaches only the caller's surface, and the two contracts change independently.
4. **A caller is a certificate of a new role, `caller`.** The coordinator approved this on 2026-11-24 for #373, as a new credential.
   - **The certificate.** `lib/identity` gains the role `caller`. The certificate's one OrganizationalUnit is `caller`, and its CommonName is the caller's name, a DNS label, for example `garam`.
   - **Issuance.** The signer issues it at `POST /v1/caller`. Only root may call it. The body is the name, an optional `days` and the certificate request. The signer takes the CommonName from the name and never from the request. The certificate carries client authentication only. It is valid 90 days by default and at most 90. Its serial is recorded as role `caller`, so it is revocable like any pair. `naructl caller credential` with `--name` writes the pair. The caller keeps it: garam keeps it in AWS Secrets Manager.
   - **What a caller may do.** A caller reaches only the Machine API, and every Machine there belongs to the caller that created it. Its records are kept under the caller's name, so another caller's Machine, snapshot or alias is unaddressable (404) and not forbidden, as orch already answers an exposer for another network's exposure ([orch.md](../orch.md) §Open questions, Exposures, where that 404 is recorded as an interpretation of ADR 0085 item 7, which says 403). A caller reads no host, no inventory, no event of the fleet and no other caller's name.
   - **Other roles.** A certificate of any other role is refused on the Machine API's listener (403). The operator API's reader, automation, operator and root roles read every caller's Machines there, and operators move them ([ADR 0110](./0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md) item 7).
5. **The resources.**
   - **Machine.** It is named by the caller (a DNS label, unique within the caller), and orch gives it an `id`, 16 random lowercase hexadecimal digits, that never changes, moves included.
     - Its spec is the image (`repository@sha256:<digest>`), `cpu`, `memoryBytes`, `rootDiskBytes`, `volumes`, the run configuration and `services`. A volume is `name`, `sizeBytes` and `mountPath`, a disk that lives as long as the Machine and moves with it. The run configuration is `command`, `args`, `env`, `user`, `workingDir` and `files`, which are mvmd's fields of Create Machine. `services` are the ports a proxy may reach (ADR 0112). The spec also carries the egress `networkPolicy`, the `shape` (item 7), and `state`, the desired `running` or `stopped`, which start and stop change and which create's comparison leaves out.
     - The root disk is made from the image at each placement and is not carried by a move. What must survive a move is on a volume.
   - **Snapshot.** A snapshot of one Machine's volumes, with its spec, named by the caller and unique within the caller, a resource of its own that outlives its Machine until the caller deletes it. It is kept on the host its Machine ran on when it was taken, and a restore from it is placed there ([ADR 0110](./0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md) item 4). It is taken from each volume by mvmd's Snapshot disk: consistent while the Machine is stopped, and crash-consistent while it runs. It is restored as a new Machine that boots from the image with volumes made from the snapshot. It holds no memory. mvmd's memory snapshot restores only onto the disks it recorded, under their identifiers (garamsh/mvmd `runtime-api.md` §Disks and resources, "A snapshot and a restore carry the Machine's disks"), so it cannot give a restored Machine volumes of its own. A memory snapshot is a later capability.
   - **Alias.** A hostname a use publishes for one of its Machines (ADR 0112).
6. **The operations.** A body is JSON. An error is `{"error", "code"}` with the status codes orch's API uses.

   | Operation | Call | Rule |
   | --- | --- | --- |
   | Create | `PUT /machines/v1alpha1/machines/{name}` | Idempotent by name: the same spec, `state` aside, answers 200 with the Machine, a different spec 409; 201 when created, with the `state` given (default `running`). The call records the Machine and returns. Placement and start follow, and the status shows them. |
   | Read | `GET .../machines`, `GET .../machines/{name}` | The spec and the status (item 8). |
   | Start, stop | `POST .../machines/{name}/start`, `.../stop` (`graceSeconds`) | Sets the desired state; a repeat changes nothing. |
   | Delete | `DELETE .../machines/{name}` | 202. The Machine and its volumes are removed, and the name is held until they are. Its snapshots stay. |
   | Snapshot | `PUT .../snapshots/{snapshot}` (`machine`), `GET .../snapshots`, `GET`, `DELETE .../snapshots/{snapshot}` | Idempotent by name: the same Machine answers 200, another 409. A snapshot outlives its Machine; `DELETE` removes it. |
   | Restore | `PUT .../machines/{name}` with `fromSnapshot` (`snapshot`) | A new Machine whose volumes are made from the snapshot. Its spec is the request's, and the snapshot's volumes must match its volumes by name and size (422). |
   | Exec | `POST .../machines/{name}/exec` (`command`, `stdin`, `timeoutSeconds`) | Returns the exit code, stdout and stderr. A running Machine only (else 409). |
   | Files | `GET` and `PUT .../machines/{name}/files?path=` | Reads or writes one file in the guest, as mvmd's file API does. A running Machine only. |
   | Watch | `GET .../events?since=<cursor>` | Each change of the caller's Machines' status after the cursor, from a log of the caller's own (the newest 20000 events and none older than 7 days, apart from the fleet's event log). A cursor older than the log keeps is 410: the client lists again. `GET .../machines` returns the cursor its listing is current at, so a list and a watch from its cursor miss nothing. A use's controller follows it and does not poll. |
   | Aliases | `PUT`, `GET`, `DELETE .../aliases/{hostname}` | ADR 0112. |

   Exec and files are answered by the member that took the call, which calls the Machine's host. They are not state and do not pass through the leader. A long exec stream is an addition for a later version.
7. **A use chooses a shape, never a host.** The `shape` is `region`, `labels` (the values a host must carry), and `gpu` (`model`, `count`; [ADR 0111](./0111-a-gpu-machine-runs-on-the-cloud-hypervisor-driver-with-whole-devices-by-vfio-on-a-bare-metal-host-pinned-to-the-devices-numa-node-with-hugepages.md)). How a shape becomes a host is [ADR 0110](./0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md). A device is part of the spec at creation. A Machine with a device is changed by deleting it and creating it again, and no operation attaches one to a running Machine.
8. **The status, and the capabilities each Machine reports.**
   - **The status fields.**
     - `phase`: `pending`, `starting`, `running`, `stopping`, `stopped`, `moving`, `failed`, `deleting` or `unknown`.
     - `reason`: why a pending or failed Machine waits, naming what of the spec or shape cannot be met ("no host fits the memory", "the snapshot's host does not answer") and no figure of the fleet's capacity. Operators read the full reason on the operator API.
     - `address`: the stable name, which a move does not change. The private address, which comes from the host and changes with it, is not in the Machine API; operators read it on the operator API.
     - `generation` and `observedGeneration`.
     - `capabilities`.
     - No host. An unreachable host shows as `phase: unknown` with the reason "the Machine's host does not answer".
   - **The capabilities.** They are computed by orch from what the Machine's host and form allow:
     - `snapshot`: `volumes` for every Machine. `memory` is added when a later implementation restores a memory snapshot onto new volumes and the host's Driver reports mvmd's `snapshot` with no device attached. `v1alpha1`'s implementation offers `volumes` only.
     - `move`: the kinds of move the Machine takes. `cold` is always present (stop, place, start, [ADR 0109](./0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md)). `snapshot` and `live` are not offered by `v1alpha1`'s implementation.
     - `resize`: `cpu` and `memory`, from the Driver's resize capabilities.
     - `gpu`: the model and count attached.

     The capabilities are values in one API, not a second API: a GPU Machine and a CPU Machine take the same calls, and a call a Machine's capabilities exclude is 409 naming the capability.

## Options considered

- **The Machine API as more routes of orch's operator API `/v1`.** One version would then cover two contracts with different callers and lifecycles. Every operator route would also sit on the listener a caller's network reaches. Rejected.
- **gRPC, as mvmd's runtime API is.** Exec streams would be natural there. But every naru API is HTTPS with JSON over mutual TLS, checked by `lib/identity`'s one verifier ([ADR 0062](./0062-every-naru-tls-peer-is-checked-against-the-signed-revocation-list-by-one-verifier-in-lib-identity-and-fails-closed-when-the-list-is-older-than-its-max-age.md)), and garam calls it over HTTP. Revisited if exec streaming needs it.
- **A unit of its own (`naru-apid`).** It would only read and write orch's etcd. A new unit is earned by a lifecycle of its own ([structure.md](../structure.md) §Boundaries and dependency direction), and this has none. Rejected.
- **The routing proxy serves the API.** The proxy is the data path (ADR 0112). E2B keeps the API and the edge apart, so that traffic never passes through the API and the API is not on the traffic's path. Rejected.
- **Kubernetes resources as the API.** #373 stops Kubernetes on naru, and the service must depend neither on garam's cluster nor on garam. Rejected.
- **Bearer tokens for callers.** They would be a second authentication system beside `lib/identity` with its own issuance, revocation and storage. Rejected.
- **The `automation` role for callers.** It reaches the whole operator API. Rejected.
- **Hosts named in the API, or a host hint.** A use would then depend on placement, and a move would change what the use sees. Rejected.

## Consequences

- garam's Workspace controller is the first caller ([machines.md](../machines.md) §The first user). A Function service later needs no change to the API, only a caller certificate.
- The API stays alpha until a second production implementation passes `machineapitest`.
- `lib/identity`, the signer and naructl gain the `caller` role, `POST /v1/caller` and `naructl caller credential`. `lib/wire` gains port 7600.
- A caller's renewal is the caller's: a 90-day pair is issued again by root before it lapses. Nothing renews it automatically.

## Signs the decision was wrong

- A use needs to know a host to do its work, for example to keep two Machines apart. The shape would then need an anti-affinity field, which is an addition, and not a host in the API.
- Callers outnumber what root can issue by hand, which would argue for an issuance an operator can delegate.
- A second production implementation cannot be written against `machines.md` and `machineapitest` alone.
