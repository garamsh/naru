# ADR 0057: an operator reaches the signer through the Node API's admin role, which forwards `/v1/signer/` to the signer's unix socket and adds no authority

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#162 under plan #113; it serves the criterion of garamsh/aurora#811 that an operator uses the signer without a login on its host. It adds a route to the Node API of ADR 0017 and leaves ADR 0040 (the signer) and ADR 0047 (no login path, no unnamed port) as they are.

## Context

- **The signer listens on a unix socket and a Node has no login.** `naructl operator add`, `signer bundle` and the other signer commands take `--socket`, a path on the signer's host. ADR 0047 gives a Node no shell, no SSH and no port its configuration does not name, so an operator has no way to reach that path.
- **The Node API already authenticates an operator.** It is served by init over mutual TLS from its own authority, and a certificate's role says whether a request is a read or a change (ADR 0017).
- **The signer does not trust its caller.** Each of its calls carries a request signed with a person's key, which the signer verifies against its authorities (ADR 0040). Reaching its socket is reach and no authority.

## Decision

1. **Every request under `/v1/signer/{path}` is forwarded to the signer's socket as `/v1/{path}`.** The method, the body, the `Content-Type` and the signed-request header `X-Naru-Request` are carried; no other request header is. The forwarded `Host` is the signer's own, and the status, `Content-Type` and body of its answer are returned unchanged.
2. **The route needs the admin role for every method,** a `GET` included, since `GET /v1/bundle` hands out the sealed authorities. A reader is refused with 403. The rebooting refusal of ADR 0022 does not apply to it.
3. **The socket is the one `spec.signer` names.** init derives it from one constant that also gives the signer's `--socket` and orch's `--signer-socket`. A Node whose configuration has no `spec.signer` answers 404, a socket that cannot be dialled 503, and any other failure of the exchange 502, each with a reason that names no path of the host.
4. **The body is streamed and bounded by what the signer reads, 1 MiB.** A larger body is 413 and is not sent on. A path that is not clean (`..`, `.`, an empty segment) is 400.
5. **The proxy gives no authority.** It signs nothing, adds no header the signer trusts and logs no body. A request without a valid signature is refused by the signer exactly as on its socket; a test runs a handler that verifies signatures behind the proxy to show it.
6. **`naructl` takes `--config <client file>` and `--node <ip[:port]>` wherever it takes `--socket`.** A command needs exactly one of `--socket` and `--config`, and `--node` only with `--config`. One chooser builds the HTTP client, over the unix socket or over the Node API under `/v1/signer`, and no command's body changes.

## Rejected alternatives

- **A TCP listener of the signer's own with its own TLS.** It is a port the Node's configuration would name beside the Node API's, a second authority and client-certificate scheme to run and rotate, and a second place to decide who may connect, for what the admin role of an existing listener already decides.
- **Ship `naru-sockproxy` to carry the socket to the operator.** It must run on the signer's host, so something must start it there, and the only way to do that on a Node is the login path ADR 0047 refuses.
- **Sign inside the Node API on the operator's behalf.** The signer's guarantee is that no process but the person's key makes a request it accepts; a Node API that signed would hold the authority this ADR is meant not to hand over.

## Consequences

- A Node API admin can reach the signer and cannot use it: every call that changes or reads an authority still needs a request signed by an operator's or root's person file, which the Node API never sees. An admin certificate and an operator credential are two things a person must hold.
- A Node API admin can send the signer unsigned requests, which it refuses. That is the surface the socket's group already had, now reached over the network by whoever holds an admin certificate.
- The signer's largest body and the name of its signed-request header are written in `lib/init/api` as well as in the signer, because `lib` cannot import `host`. A change to either must change both; a test of the proxy holds the bound.
- `lib/init` requires `lib/identity` for its tests, to run a handler that verifies signatures.

## Signs the decision was wrong

- A second route to reach another daemon's socket is asked for, which would say the forwarding belongs in a general facility with its own configuration.
- An admin certificate is issued as freely as a reader's because it is thought harmless, which would say the role is no longer the gate this decision relies on.
