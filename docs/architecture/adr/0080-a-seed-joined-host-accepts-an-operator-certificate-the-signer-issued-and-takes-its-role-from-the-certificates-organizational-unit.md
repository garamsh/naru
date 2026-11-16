# ADR 0080: A seed-joined host accepts an operator certificate the signer issued and takes its role from the certificate's organizational unit

> Status: accepted
> Date: 2026-11-16

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#238, part of goal garamsh/naru#235. It changes [host.md](../host.md)'s roles and `naructl` client, and [config.md](../config.md)'s host commands.

## Context

- **A seed-joined host has no client file.** `naructl gen host-client` writes a pair under the host authority with the Organization `naru:host-provisioner` or `naru:host-reader`, and needs the host authority's key. A host that joined from an install seed is configured by orch, and the operator keeps no key of that authority per host.
- **The host trusted the operator's certificate and refused it.** A joined host's client CA is orch's callers bundle (`spec.orch.ca`). The certificates the signer issues to people and tools (`naructl operator add`) chain to its operator authority, so they verified at TLS. `roleOf` in `host/internal/api/roles.go` mapped only `orch` and the two Organizations, so the role was none and the answer 403.
- **`naructl` could not present such a certificate.** Its host commands read a client file only.

## Decision

**A seed-joined host accepts a certificate issued under the signer's operator authority, and the role is the certificate's one organizational unit.**

1. **The credential is the person file of `naructl operator add`.** No second pair is made for the host API.
2. **Roles.** `root` and `operator` are `provisioner`; `reader` and `automation` are `reader`; `host`, an unknown unit or more than one unit is none (403). `orch` stays `provisioner`, and the Organizations `naru:host-provisioner` and `naru:host-reader` are unchanged.
3. **The host's client CA must hold the operator authority.** It is orch's callers bundle, so `spec.orch.ca` holds the operator authority beside the orch authority, as the output of `naructl signer ca` has them. The profile in `lib/layout` is unchanged.
4. **`naructl` host commands take `--identity <person file> --passphrase-file <file> --host-ca <host authority PEM> --endpoint <address>`** as the alternative to `--config`, with `--server-name <h-id>` because a joined host's certificate names only its host ID. Exactly one of the two forms is given.

## Consequences

- **An operator reads and provisions a joined host with the certificate they already hold.** Revoking the certificate (`naructl operator revoke`) ends it for the hosts as for orch.
- **`automation` is a reader here.** A tool that must write to a host takes an `operator` certificate; the mapping is a choice that a later ADR may change.
- **A deployment whose `spec.orch.ca` lacks the operator authority gets a handshake refusal**, as before this decision.
- **`root` provisions hosts.** The host API gives root nothing beyond operator.
