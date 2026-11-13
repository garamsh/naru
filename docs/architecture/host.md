# Host API

What `naru-hostd` is: the service that creates, shows and destroys microVMs on a Node through mvmd's local runtime API, and what it asks of the Node's configuration. This is the interface a control plane above the Node (garamsh/aurora) calls; gaon's own Node Agent is a separate caller of the same mvmd.

## Current decisions

- **`naru-hostd` is a unit.** It is the module `host/`, with one binary, `cmd/naru-hostd`. The image carries it as `/bin/naru-hostd`. A Node runs it only when its configuration has `spec.host`; naructl writes the service into `spec.services` then, after mvmd, and init supervises it like any other service ([ADR 0028](./adr/0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md)).
- **Where it listens.** HTTPS over mutual TLS 1.3 on `0.0.0.0:7443`. The client's certificate must verify against `--client-ca`; the role is the client certificate's Organization, the highest of those it carries (`naru:host-provisioner` is `provisioner`, `naru:host-reader` is `reader`), and a certificate with neither is refused with 403. The authority is the host authority, `<ca-dir>/host/ca.pem`, which is neither gaon's CA nor the Node API's.
- **What each role may do.**

  | Endpoint | reader | provisioner |
  | --- | --- | --- |
  | `GET /v1/host` | yes | yes |
  | `GET /v1/microvms`, `GET /v1/microvms/{name}` | yes | yes |
  | `POST /v1/microvms` | 403 | yes |
  | `DELETE /v1/microvms/{name}` | 403 | yes |

- **`GET /v1/host`** returns `region`, `labels`, the Node's `capacity` and what is `free` (CPU, memory and state disk; CPU and memory of running Machines are subtracted), and mvmd's `version` and `driver`. Region and labels are read from `/run/naru-hostd/host.json` on every request, so a changed label shows at the next request.
- **`spec.host`** is `region` (a DNS label) and `labels` (Kubernetes label keys and values, at most 64). Parsing derives the file `/run/naru-hostd/host.json` from it as an entry of `spec.files`, so the apply engine writes it when `spec.host` changes and no service restarts: a label change is live ([config.md](./config.md), [init.md](./init.md)). An entry the user wrote at that path is replaced by the derived one.
- **`POST /v1/microvms`** takes `name` (a DNS label, which is mvmd's Machine ID), `image` (`repository@sha256:<digest>`), `cpu`, `memory` and `rootDiskBytes` (bytes), `disks` (`name`, `sizeBytes`, `readOnly`) and `networkPolicy` (`egress` and `ingress` rules of `prefix`, `protocol` and `ports`). It pulls the image, creates the disks as `<name>.<disk>`, creates the Machine and starts it, and answers 201 with the microVM. `env` and `command` are refused with 422: mvmd at the pinned commit has neither field.
- **A POST is idempotent by name, without state in hostd.** A request that names a Machine mvmd holds is compared with what mvmd recorded (image digest, CPU, memory, root disk size, disk sizes, network policy): the same request answers 200 with the same microVM, and a different one answers 409. A Machine that an interrupted request left Created or Stopped is started again.
- **`DELETE /v1/microvms/{name}`** stops the Machine, deletes it and the disks named `<name>.*`, and answers 204. A name mvmd does not hold answers 404 after removing the unattached disks that carry the name's prefix, so a retry finishes what an interrupted delete left.
- **Errors** are JSON `{"error": "..."}`: 404 not found, 409 conflict or precondition, 422 invalid request, 503 mvmd unavailable, 504 mvmd timed out, 502 any other mvmd failure.
- **Fencing.** mvmd starts no Machine without an armed fencing deadline. `naru-hostd` renews it (length 10 minutes, every minute, retried after 5 s), with a number above mvmd's last, read from `InspectDeadline` at start and again after a failed renewal.
- **Keys.** naructl issues the serving pair and writes it into `spec.files` (`/run/naru-hostd/tls.crt`, `tls.key` at `0600`, and `client-ca.crt`), as it does for the Node API. The host authority's key stays with the operator and is in no Node's user-data. The pair is valid one year and is not renewed.
- **`naructl`** is the client: `gen host-ca`, `gen host-client --role provisioner|reader`, `gen node --host-region --host-label`, `host`, and `microvm create|get|list|delete` ([config.md](./config.md)).

## Rationale

- [ADR 0028](./adr/0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md): why a unit of its own, why mutual TLS from a separate authority with two roles, why idempotency and the 409 come from mvmd's record and not from state of hostd's, why `spec.host` reaches the running service as a derived file, and the signs it was wrong.
- [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md): the pattern the host API repeats.

## Open questions

- **`env` and `command`.** The issue asks for them; mvmd's runtime API at the pinned commit has no such fields, and hostd refuses them. They are added when the pin moves to a commit that has them.
- **Removing `spec.host`.** The apply engine removes no file, so the derived file stays on the Node's tmpfs until the next boot; the service keeps running until it is removed from `spec.services`.
- **Renewing the serving pair.** It is valid one year, as the Node API's is, and no one renews it.
- **Sharing mvmd with gaon's Node Agent.** Both renew mvmd's one fencing deadline. hostd re-reads the deadline after a failure, so the two converge, and what the Node Agent does under that is gaon's to say.
