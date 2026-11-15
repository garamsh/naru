# ADR 0060: the gaon Node layout is the library `lib/layout`, called by naructl and by orch when it builds a joining host's configuration

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#163, part of plan garamsh/naru#113; it carries decision 4 of garamsh/aurora#811 comment 6886769198. It supersedes in part [ADR 0015](./0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md), whose title and item 1 name `naructl` as the single owner of the layout; the rest of ADR 0015 stands, and so does [ADR 0019](./0019-the-gaon-topology-and-a-nodes-credential-mode-are-defined-once-in-naructls-layout-and-the-harness-reads-the-topology-from-naructl-topology.md) apart from where the layout lives.

## Context

- **Orch is to build a joining host's configuration at admission** (garamsh/naru#165): from the fleet's host profile and the host's own facts it writes the user-data init applies as revision 1. That is the host Node's layout, `spec.host` and its serving pair among it, which `naructl gen node` writes today.
- **The layout was `naructl/internal/layout`**, a package no other unit may import ([ADR 0039](./0039-init-dissolves-into-lib-init-called-by-host-and-node-and-controlplane-into-lib-vip-and-node-and-a-check-fails-an-import-between-units.md): units never import each other, and an internal package is not importable from outside its module).
- **A second writer of the layout drifts** (ADR 0015, Context). Orch writing its own copy of `spec.host` would be the failure ADR 0015 was made to prevent.
- **The layout calls `naructl/internal/pki`**, the CA, certificate and bootstrap-token code, which `naructl`'s own commands and client also call.

## Decision

**The layout is the module `lib/layout`, and `naructl` and `orch` call it. `lib/layout` is the one writer of a Node's user-data, as `naructl/internal/layout` was; ADR 0015's single owner is the library, not the binary.**

1. **`lib/layout` is a module of `lib/`** ([structure.md](../structure.md)): package `layout`, moved unchanged from `naructl/internal/layout`, with its own `go.mod`, listed in `go.work`. A module that requires it carries a `replace` to `../lib/layout`.
2. **`lib/layout/pki` moves with it**, from `naructl/internal/pki`: the layout calls it, and a `lib/` module imports no unit. It is a second package of the same module, a family of packages that belong together as `lib/init` is, and `naructl` imports it from there for its own commands.
3. **The move changes no behaviour.** The user-data of a control-plane Node and of a host Node, from one CA directory, is byte-identical before and after, with the random bootstrap token and the host's serving pair masked.
4. **`naructl` stays the only program that writes a Node's user-data for an operator**, and `tooling/qemu` still runs it as a binary and imports none of it. It never imported the layout, so its module is unchanged.
5. **Orch's call is added with garamsh/naru#165**, which is the reason for the move; this decision makes the layout importable and adds no caller.

## Consequences

- **A change of a service's flags or paths is still made once**, in `lib/layout`, and `naructl`, orch, the QEMU tests and the release all pick it up.
- **A change to `lib/layout` can break two units at once.** Each module that requires it runs its own tests with the change.
- **`naructl/internal/pki` no longer exists.** The package is public now, so its API is held to what a second caller may depend on.
- **`lib/layout` imports no unit**, which `make lint-imports` checks.

## Signs the decision was wrong

- Orch needs a form of the layout that `naructl` never writes and changes it for itself, which would say the layout is two things and the part orch calls belongs in its own module.
- A change to `lib/layout/pki` made for orch's sake alone, which would say the PKI belongs in its own module under `lib/`, not beside the layout.
