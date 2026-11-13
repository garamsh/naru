# ADR 0028: naru-hostd serves a host API over mutual TLS from its own authority and creates microVMs through mvmd's runtime API

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#88, on the path of garamsh/aurora#804. It repeats the pattern of [ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) for a second API and amends neither it nor [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md).

## Context

- **Something above the Node must create microVMs on it.** gaon's Node Agent creates Machines for gaon's own resources. A control plane that is not gaon (aurora) needs to create, read and destroy microVMs on a chosen Node, and to know the Node's region, labels and free capacity to choose it. mvmd's runtime API is a local gRPC socket, which is not reachable from another host.
- **mvmd's API asks things of its caller.** At the pinned commit `d189133bba8d842d2bcb20046f39fe03161efeb1` (v1alpha2) the client chooses the Machine ID and the request ID; `CreateMachine` needs a prior `PullImage`; `StartMachine` needs a fencing deadline that is armed, renewed with numbers that only increase; a Machine is deleted only when Created, Stopped or Faulted; a Machine ID may not hold `/`, is at most 250 bytes, and is not `tmp` or `tmp-*`. The request has no environment and no command.
- **The Node API is init's.** Init serves it from the PID 1 process ([ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)). A process that holds a gRPC connection to mvmd and renews a deadline on a timer does not belong in PID 1.
- **A configuration change should not need a reboot.** An operator changes a Node's labels as the cluster changes. The configuration's apply engine already writes a changed `spec.files` entry without a reboot ([ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md)).
- **Where the authority's key lives is the lesson of the Node API.** naructl issues the Node API's serving pair and carries no authority key to a Node.

## Decision

1. **`naru-hostd` is a unit and a service.** It is the module `host/`, binary `cmd/naru-hostd`, built into the image as `/bin/naru-hostd`. It runs as a service in `spec.services` of a Node whose configuration has `spec.host`, started after mvmd, and not otherwise.
2. **The host API is HTTPS with JSON over mutual TLS 1.3 on port 7443**, from a host authority of its own, `<ca-dir>/host/ca.pem` and `ca.key` (`naructl gen host-ca`), which is neither gaon's CA nor the Node API's. The role is the client certificate's Organization: `naru:host-provisioner` for `provisioner`, `naru:host-reader` for `reader`, the highest if both, 403 if neither. A reader reads; a provisioner also creates and deletes.
3. **Endpoints.** `GET /v1/host`, `GET /v1/microvms`, `GET /v1/microvms/{name}`, `POST /v1/microvms`, `DELETE /v1/microvms/{name}`, with the bodies and the status codes of [host.md](../host.md).
4. **The Machine ID is the microVM's name**, a DNS label, which satisfies mvmd's rules. A disk's ID is `<name>.<disk>`.
5. **A POST is idempotent by name, and hostd keeps no state for it.** hostd compares the request with the Machine mvmd holds under the name, and with its disks: equal is 200 with the same microVM, different is 409. A Machine an interrupted POST left Created or Stopped is started. A DELETE of a name mvmd does not hold removes the unattached disks that carry `<name>.` and answers 404.
6. **hostd renews the fencing deadline**, length 10 minutes, every minute, with a number read from `InspectDeadline` at start and again after any failed renewal.
7. **`env` and `command` are refused with 422**, because mvmd at the pinned commit cannot take them. They are added when the pin moves.
8. **`spec.host` is `{region, labels}`.** Parsing derives `/run/naru-hostd/host.json` from it as an entry of `spec.files`, mode `0644`, replacing any entry at that path; hostd reads the file on every `GET /v1/host`. The apply engine then writes the file on a change and restarts nothing, so a label change is live and `spec.host` is not in the reboot-only class.
9. **naructl issues the serving pair**, as it does for the Node API: certificate and key at `/run/naru-hostd/tls.crt` and `tls.key` (`0600`) and the client CA at `client-ca.crt`, as `spec.files`. The host authority's key is not carried to a Node. The pair is valid one year and is not renewed.
10. **naructl is the client**: `gen host-ca`, `gen host-client`, `gen node --host-region --host-label`, `host` and `microvm create|get|list|delete`.

## Consequences

- A caller above a Node creates and destroys microVMs over the network with a certificate it can be given or refused, and learns where a Node is and what it has free.
- hostd and gaon's Node Agent both renew mvmd's one deadline on a Node that runs both. Each renewal that finds a number at or below mvmd's last fails once, and hostd re-reads and goes on; the two cannot hold the deadline apart, and neither starts a Machine on a lapsed one.
- hostd has no database. What a name means after a crash is what mvmd holds, so a restart needs no recovery. A create that fails between its steps leaves disks and, at worst, a Created Machine, which the retry of the same request finishes and a DELETE removes.
- Idempotency is a comparison of fields, so it stops at what mvmd records: a request that differs only in a field mvmd does not return would read as the same one. The fields hostd sends are the fields it compares.
- `spec.host` appears in `GET /v1/config` as the derived file, and a file the user wrote at `/run/naru-hostd/host.json` is replaced silently.
- Removing `spec.host` removes no file, as the apply engine removes none: the file stays until reboot and the service until it is removed from `spec.services`.
- The serving pair expires after a year. A Node whose pair has expired needs a new user-data or an apply of new files, as the Node API's does.

## Signs the decision was wrong

- Callers need `env` or `command` and the pin has not moved: the 422 is then the decision's cost, and a field mvmd lacks is mvmd's to add, not hostd's to emulate.
- A create that failed midway is found to leave something the retry and the DELETE do not clear, or two different requests are found to read as the same one: idempotency needs the record that this decision does without, held by hostd on the data disk.
- hostd and the Node Agent are seen to fight over the deadline, with Machines faulting for want of it: the deadline needs one owner on a Node.
- Operators run the host API on Nodes with no way to give a file the apply engine would write, or expiry is met in the field: the pair needs renewal, as `spec.pki` certificates have.
- A caller needs finer roles than reader and provisioner, such as a provisioner of one region: the role is then more than an Organization, and this decision's two roles are too coarse.
