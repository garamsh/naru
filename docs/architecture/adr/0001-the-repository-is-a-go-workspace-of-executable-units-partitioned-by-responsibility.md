# ADR 0001: The repository is a Go workspace of executable units, each its own module, partitioned by responsibility

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#1. The direction is the project owner's, stated in the naru PM session of 2026-11-11 and recorded on that issue. The precedent is garamsh/garam at `6e80e2a4f775a5ddc252884492e4540e048242d1` (its `docs/architecture/structure.md` §Services, ADR-0125 and ADR-0132) and garamsh/gaon#1, which took the same decision.

## Context

- **naru is several executables in one repository.** It is the node OS: the OS image, its own init as PID 1, a small node API (apply config, upgrade, reset, status) and the installer. It builds and manages the host kernel and the guest kernel. The decisions are garamsh/discussions Discussion #3 decisions 3, 4 and 6.
- **gaon sits on naru's boundary.** gaon calls naru's node API and naru does not know gaon's resource API (Discussion #3 decision 5). gaon therefore requires a node API client from naru as an external module.
- **The repository holds non-Go build inputs** (kernel configuration, image definition) beside the Go code. Where they live is not decided here.
- **The structure must cover** an init, a node API server, an installer and a client module in one repository, so that a change lands in one folder and a unit's pull request touches only that unit.
- **Candidates read** (`docs/architecture/structures/` at `35a77f349a8ab3a8b6adf356291cb842e3733782`): layered, hexagonal and domain.
  - Layered divides by technical responsibility, so one feature crosses every layer; naru's units differ in what they run, not in a technical tier.
  - Hexagonal divides an application from its technologies. It could apply inside a unit, which this decision does not constrain, and it says nothing of how several executables relate.
  - Domain divides by subject matter, one folder per boundary reached through a declared surface, and leaves open whether a boundary may depend on a sibling.

## Decision

**The structure builds on the domain structure, with the unit as the boundary: the repository is a Go workspace in which each executable unit is a top-level folder holding one Go module, and units reach each other only through a published API.**

1. **One Go module per executable unit**, a top-level folder with its binary under `cmd/`, as `docs/convention/stack-go.md` §1 lays a service out.
2. **A `go.work` at the root names every Go module.**
3. **A unit reaches another only through what that unit publishes.** Reaching into another unit's `internal/` packages is a defect even where it builds. This is the side taken on the point the domain structure leaves contested, whether a boundary may depend on a sibling at all.
4. **A library linked by more than one unit, or required by another project, is a module of its own under `lib/`.** There is no `shared/`, `utils/` or `common/`, and a package that one unit alone imports belongs to that unit.
5. **The node API client that gaon requires is such a module.** gaon requires it as an external module, as garamsh/mvmd's `lib/runtimeapi` is for gaon.
6. **A unit's folder is created by its first pull request.** Bootstrap creates none.

`docs/architecture/structure.md` carries the rules. Where the non-Go build inputs live is not decided and waits under its open questions (`docs/architecture/README.md` rule 7).

## Consequences

- A change to one unit touches one folder, and a unit's owner is the folder's owner.
- A unit is tested and built against the API another unit publishes, so gaon can depend on naru's client module without depending on naru's other units.
- A second consumer of a package forces its extraction into `lib/` as a module, which costs a `go.mod` and a `go.work` entry.
- Every Go entry point has to cover every module `go.work` names, because no root module exists.
- Which structure applies inside a unit is not decided here: the unit's own files follow `docs/convention/stack-go.md` §1.
- The non-Go build inputs have no home until a decision gives them one.

## Signs the decision was wrong

Carried from `docs/architecture/structures/domain.md` at `35a77f349a8ab3a8b6adf356291cb842e3733782` §Signs the choice was wrong, read with the unit as the boundary.

- Most changes touch several units at once, and a pair of units that always move together cannot be released independently.
- The same fix is applied once per unit, and a unit keeps being missed.
- The shared code every unit imports (`lib/`) is growing faster than any unit.
- Which unit a new file belongs to cannot be settled without asking someone.
- A unit's public surface has grown until it names what the unit keeps inside.
