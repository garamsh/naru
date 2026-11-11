# Structure

The shape naru is partitioned into, what may reference what, and when a new unit is earned.

## The shape

naru is divided by responsibility into executable units, one top-level folder per unit. A unit is a Go module with its binary under `cmd/`, or a library module under `lib/`. This is the domain structure with the unit as the boundary: each folder is named for what it runs or publishes, never for a technical role.

Nothing here states what goes inside a unit, or how the units are deployed. A unit's own files follow `docs/convention/stack-go.md` §1; deployment is a separate decision.

## Units

The repository holds these responsibilities (garamsh/discussions Discussion #3 decisions 3, 4 and 6). No unit's folder exists yet and none has its name decided here; each is created by the pull request of the issue that owns it.

- **An init that runs as PID 1**, a binary.
- **A node API server**, a binary: apply config, upgrade, reset, status.
- **An installer**, a binary.
- **A node API client**, a library module under `lib/`, which gaon requires as an external module (Discussion #3 decision 5).
- **Non-Go build inputs**: kernel configuration for the host and guest kernels, and the image definition. Where they live is an open question below.

The rules for units:

- **A top-level folder is one responsibility**: an executable unit, or a supporting responsibility (`lib/`, `docs/`).
- **One Go module per executable unit**, its binary under `cmd/<binary>/main.go`.
- **A `go.work` at the root names every Go module**, and the root holds no Go package.
- **A library linked by more than one unit, or required by another project, is a module of its own under `lib/`**, its package at the module root as `docs/convention/stack-go.md` §1 lays out a module that ships no binary. `lib/` holds no binary. There is no `shared/`, `utils/` or `common/`, and a package that one unit alone imports belongs to that unit.
- **The node API client gaon requires is a module under `lib/`**, as garamsh/mvmd's `lib/runtimeapi` is for gaon. gaon requires it as an external module and never copies it in.
- **A unit's folder is created by its first pull request**, and not before.
- **Dependencies between projects are one-way**: gaon calls naru's node API, and naru does not know gaon's resource API.

## Boundaries and dependency direction

- **A unit is the boundary.** One folder per unit, named for what it runs or publishes and never for a technical role.
- **A unit is reached only through what it declares public.** Everything else it holds is internal, and reaching past that surface is a defect even where the language permits it.
- **A unit reaches another only through what that unit publishes.** Reaching into another unit's `internal/` packages is a defect even where it builds. This is the side taken on whether a boundary may depend on a sibling, which the domain structure leaves contested ([ADR 0001](./adr/0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md)).
- **Shared code sits below every unit, never beside one.** What two units both need is extracted into `lib/`, or owned by one unit and reached through what it publishes; it never becomes a sibling folder no unit owns.
- **A cycle between units means the split is wrong**, and so does a file that belongs in two of them.
- **A new unit is earned** by a responsibility with its own lifecycle and vocabulary, which something outside it depends on, and whose published surface can stay stable while its inside changes.

## Where the composition site lives

- **The application has one composition site.** It is the one place that constructs concrete implementations, and it decides which implementation each declared abstraction receives.
- **Every other unit receives what it depends on and constructs none of it**, whatever the shape makes the unit: a domain, a layer, or an adapter.
- **The composition site stays thin.** It constructs and passes; it decides nothing else.

Each executable unit has its own site: its `cmd/<binary>/main.go`, or the package that file calls where a test outside `cmd/<binary>/` has to reach it (`docs/convention/stack-go.md` §Project envelope).

## Decisions this rests on

- [ADR 0001](./adr/0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md): the repository is a Go workspace of executable units, each its own module, partitioned by responsibility. It records the structure taken, the candidates read, and the signs that it was wrong.

## Open questions

- **Where the non-Go build inputs live.** The kernel configuration and the image definition are not Go modules, and no rule here gives them a folder. Until a decision does, this binds nothing.
- **How a unit is divided inside.** `docs/convention/stack-go.md` §1 lays out a service module, and no decision here adds to it. Until one does, this binds nothing.
