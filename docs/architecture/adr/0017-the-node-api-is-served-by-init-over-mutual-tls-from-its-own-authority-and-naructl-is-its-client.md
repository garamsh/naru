# ADR 0017: the Node API is served by init over mutual TLS from its own authority, and naructl is its client

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#46, part of goal garamsh/naru#44. The direction is decisions D1 to D4 and D6 of the PM's design comment, https://github.com/garamsh/naru/issues/44#issuecomment-6824452883, taken under the grant of that goal. Details the comment left open were decided in the PM's review of garamsh/naru#50 and are recorded as written. Talos is cited by concept only (a machine daemon, an API proxy, a client configuration file). This ADR supersedes nothing: ADRs 0012 to 0016 stand, and ADR 0002 and ADR 0008 are filled in and not contradicted (§Consequences).

## Context

- **ADR 0002 left the Node API undecided.** It says naru owns "a small node API (apply config, upgrade, reset, status)", that it is the only contract between gaon and naru, and that its shape and PKI wait under [boundaries.md](../boundaries.md) §Open questions.
- **Init owns what the API reports and changes.** The service state is `init/internal/service` (`Start`, `Stop`, `Restart`), the log is the console's kept lines (`init/internal/console`, [ADR 0009](./0009-init-re-prints-the-console-log-at-a-fixed-interval-between-numbered-markers.md)), and the configuration is parsed and validated by `init/internal/config`. At commit `5b42b429c6b41447f2ea267fe606c2b5d44ea63e` init has no listener, and a service's output reaches the kept log without a mark of which service wrote it (`init/internal/linux/launcher.go` copies each service's output to the console).
- **A Node cannot be read or changed today without a new NoCloud drive and a reboot.** `naructl` writes user-data and nothing else reaches a running Node.
- **The release's test user-data must publish no new key.** [ADR 0016](./0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md) item 5 publishes the build's test CA key, and an API authority key published the same way would let anyone reach every released Node's API.
- **gaon's API is HTTPS with JSON and mutual TLS**, which a Go standard-library server and client speak without a gRPC or protobuf toolchain in init.

## Decision

**Init serves the Node API in-process, over HTTPS with JSON and mutual TLS 1.3, trusting only a Node API authority that is not gaon's CA; `naructl` makes that authority and each Node's serving pair, and is the client.**

1. **Served by init, in-process.** The API is a package of init, not a second process and not a second unit. It reads the service manager, the kept log and the configuration store directly. Revisit when a Node must forward a call to another Node (the discovery follow-up, [target.md](../target.md)).
2. **Listener.** TCP, default `0.0.0.0:50000`, HTTPS, TLS 1.3 only, a client certificate required and verified against `spec.api.clientCA`. A client without a certificate that `clientCA` issued fails the handshake and sees no HTTP status.
3. **`spec.api` is new and optional.** It holds `listen`, `clientCA` (PEM), `certificate` (PEM) and `key` (PEM). Without it init serves no API, logs that the Node API is off, and boots as v0.3.0 does, so v0.3.0 user-data boots unchanged. The release's user-data has no `spec.api` ([ADR 0016](./0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md) §Decision item 5 and its "no new keys" stand).
4. **The authority is separate from gaon's CA and stays off the Node.** `naructl gen api-ca --ca-dir D` makes `D/api/ca.pem` and `D/api/ca.key`, independently of gaon's CA. `naructl gen node` adds `spec.api` when `D/api/` exists: the serving pair is issued from that authority and names the Node's addresses and `127.0.0.1`, and `clientCA` is the authority's certificate. No Node carries the authority's key.
5. **Authorisation is by the client certificate's Organization.** `naru:admin` may read and apply, `naru:reader` may only read, and any other certificate that passed the handshake gets 403. A certificate with several Organizations has the highest role among them
6. **v1 endpoints** (reboot, reset and upgrade endpoints belong to the follow-ups):

   | Endpoint | Role | Returns or does |
   |---|---|---|
   | `GET /v1/version` | reader | naru version, boot ID (`/proc/sys/kernel/random/boot_id`), uptime |
   | `GET /v1/services` | reader | each service's name, state, PID, restart count, start time |
   | `GET /v1/services/{name}/logs` | reader | the kept lines that service wrote; `?follow=true` streams them |
   | `GET /v1/config` | reader | the stored configuration with its `revision`, private material redacted |
   | `POST /v1/config` | admin | apply a configuration change ([ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md)) |

7. **Redaction in `GET /v1/config`.** `spec.api.key`, every `spec.pki.authorities[].key`, and the `content` of every `spec.files` entry whose mode gives no read to group or other are replaced by the string `REDACTED`. A request that carries `REDACTED` in such a place is refused with 422.
8. **Wire shapes the design leaves open** (the least that serves #47 to #49). JSON, field names in lower camel case, as follows:
   - `GET /v1/version`: `{"version": string, "bootId": string, "uptimeSeconds": number}`.
   - `GET /v1/services`: `{"services": [{"name", "state", "pid", "restarts", "startedAt"}]}`; `state` is `running`, `backoff` (waiting to restart) or `stopped`; `pid` is 0 unless running; `startedAt` is RFC 3339.
   - `GET /v1/services/{name}/logs`: `text/plain`, one line per line, the service's lines in the order kept; with `follow=true` the response stays open and each new line is flushed until the client closes. An unknown name is 404.
   - `GET /v1/config`: `{"revision": number, "config": <NodeConfig as JSON>}`.
   - Every refusal and failure: `{"error": string, "fields": [string]}` with `fields` naming the offending fields where there are any, and 409 also carries `"revision"`, the current one.
   - Status codes: 200, 400 (a body that is not JSON or not the request's shape), 403, 404, 409, 422, 500.
9. **`naructl` is the client** (the Talos client-configuration analogue). `naructl gen api-client --role admin|reader` writes a client file (mode `0600`): endpoints, the authority's certificate, a client certificate whose Organization is `naru:admin` or `naru:reader`, and its key. The commands `version`, `services`, `logs <svc> [-f]`, `get config` and `apply -f FILE [--mode no-reboot|staged] [--dry-run]` take `--config <client file>` and `--node <address>`. The exact names and flags are the worker's, subject to review; a 403, 409 or 422 is one line and exit code 1.
10. **No SSH, no shell on the Node.** The Node API is the only way in, which is already so: `image/initramfs` names no SSH daemon.

## Consequences

- **A running Node can be read and, with ADR 0018, changed without a new drive or a reboot**, by anyone holding a client certificate from the API authority.
- **Two authorities exist, and the operator keeps both keys.** The API authority's key never leaves the operator; losing it means regenerating `spec.api` for every Node and applying that by new user-data.
- **ADR 0002 is filled in, not contradicted.** Its open items, the API's shape and its PKI, are decided here for the operator's use. Its `lib/` client for gaon is not built by this decision: the client is `naructl`, and a library module for gaon waits for gaon to ask (ADR 0002 §Consequences, "the API grows by requests from gaon").
- **ADR 0008 stands.** NoCloud remains how configuration reaches a Node; ADR 0018 adds what happens after.
- **The kept log needs a mark of the service that wrote each line.** Today it has none, so serving a service's logs means init tags lines at the launcher; the tag must keep ADR 0009's markers and the judge's reading of the console unchanged, which #47 shows.
- **A Node's serial console is not the API's channel.** A Node with an invalid configuration serves no API (init stops before any service starts, [init.md](../init.md)); recovering it is the reset and recovery follow-up.
- **Init holds a network listener and a TLS stack.** It is the one process on the Node that parses requests from the network, so init's attack surface grows by the API's handlers; they are limited to the endpoints above.

## Signs the decision was wrong

- A defect in the API server stops PID 1, which would say the API belongs in a process init supervises.
- Init needs an internal IPC to hand the API something it owns, which would say the same.
- A Node has to forward a call to another Node, and init cannot do it without growing logic that ADR 0002 item 6 keeps out of init.
- gaon needs the API from a Node that only has gaon's CA, and two authorities become the obstacle and not the protection.
- An operator needs a verb that a read-only or an admin role cannot express, so Organization-based roles are too coarse.
