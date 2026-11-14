# ADR 0039: init dissolves into `lib/init` called by `host/` and `node/`, `controlplane/` into `lib/vip` and `node/`, and a check fails an import between units

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#116, the first two steps of the plan in garamsh/naru#113. It narrows [ADR 0001](./0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md)'s side on whether a unit may reach another, and it moves what [ADR 0004](./0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md), [ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) and [ADR 0024](./0024-the-control-plane-is-three-etcd-members-with-tls-on-both-ports-and-a-virtual-address-held-by-naru-vip-behind-a-fence-on-etcds-ports.md) placed, by reference: their bodies are not edited, and what they say about behaviour holds with the paths below read in place of theirs.

## Context

- **The plan puts the units under three names.** The plan in #113 divides naru into `host/` (the host), `node/` (the OS a microVM boots) and `orch/` (the orchestrator), with `naructl/` and `tooling/`, over `lib/`, and says no unit imports another.
- **Two PID 1s need the same init.** The host runs `naru-init` and so does a node, and today one module, `init/`, holds all of it behind `internal/`, which no other module may import.
- **`controlplane/` holds two things that belong to different owners.** The VIP election is wanted by the node now and by `orch/` for its own address; the fence's rules are the node's alone.
- **Nothing enforces a boundary yet.** `internal/` stops a package being imported across modules, but not a public package of one unit being imported by another.

## Decision

1. **`init/` dissolves into the library module `lib/init`,** `github.com/garamsh/naru/lib/init`. Each `init/internal/<pkg>` is the public package `lib/init/<pkg>`, and `init/internal/pkitest` is `lib/init/pkitest`. The body of `init/cmd/naru-init` is the package `lib/init/run`, which exports `Main()`; it has no `main` function of its own.
2. **Two thin binaries call `run.Main()`:** `host/cmd/naru-init`, the host's PID 1, and `node/cmd/naru-init` in the new module `node/` (`github.com/garamsh/naru/node`), the node's. The image keeps installing one `/init`, built from `host/cmd/naru-init`, and the node initrd of [ADR 0034](./0034-a-node-boots-as-a-microvm-from-a-guest-kernel-and-initramfs-the-image-carries-as-a-boot-set.md) is the same list without some entries, so it carries that binary too. `node/cmd/naru-init` is built by `make build` and is not installed in an image yet: the two are the same code, and the node initrd takes the node's binary when the image is split (decision 5).
3. **The plan's decision 1, hostd becoming PID 1, is deferred.** Two things decide it: the deadline of this round, and that a separate process keeps hostd restartable, since init supervises it with backoff and a crash of hostd does not take the host down. It is taken up when there is a reason to give that up.
4. **`controlplane/` dissolves.** `controlplane/internal/vip` is the library module `lib/vip` (`github.com/garamsh/naru/lib/vip`, its package at the module root); `cmd/naru-vip` and `cmd/naru-fence` are `node/cmd/`; `controlplane/internal/fence` is `node/internal/fence`.
5. **`lib/iso9660` stays where it is, and the split of `image/` by unit is deferred.** The image is one initramfs built from one definition, and splitting it is a decision of its own.
6. **`make lint-imports` fails when a module under `host/`, `node/`, `orch/`, `naructl/` or `tooling/` imports a package of another top-level unit, or when a module under `lib/` imports a unit.** Only `lib/` and external modules are allowed from a unit, and the unit is the first path element of the module's folder, so `tooling/` is one unit. `make ci` runs it, and `.github/workflows/ci.yml` runs the same command on every pull request and push, with read-only permissions and each action pinned by commit.
7. **The check is `tooling/importcheck`, a module of its own, and it reads imports with `go/parser`,** not with `go list -deps -json`. `go list` reads only the files the build constraints of one platform select, and no test file's imports unless asked to: a file built only with a tag, or a test file, could import another unit and pass. The parser reads every `.go` file of every module found by its `go.mod`, build-tagged and test files included, and the check prints how many files and modules it read, so a tree it read none of cannot pass silently. A tree with no `go.mod` is an error.
8. **A module that requires a `lib/` module carries a `replace` to its folder,** as `host` and `tooling/qemu` do for `lib/iso9660`, so a build with `GOWORK=off`, which the tests of `naructl` and `tooling/qemu` use to build `naru-init` as a release would, resolves it.
9. **`lib/init` is a family of public packages,** not one package at the module root as `docs/convention/stack-go.md` §1 lays out a library module. The packages are those of `init/internal/`, kept as they were, and collapsing them into one would be a rewrite of the code. This is raised as a convention concern in the pull request.

## Consequences

- A package of `lib/init` is public, so its exported names are an interface another unit may rely on. The check keeps `lib/` from importing a unit, but nothing yet narrows what a unit may import from `lib/init`.
- Both PID 1s are the same code. A difference between the host's and the node's is a flag or the configuration, not a fork of the package, until a reason to fork is met.
- A module that imports `lib/init` or `lib/vip` carries one more `replace`.
- `orch/` was created by garamsh/naru#118 while this decision was in review, and the check treats it like any other unit: it reads it with the rest, and `host/` reaches `orch/`'s API through `lib/orchapi`, which is the shape the check allows.
- The tests that built `naru-init` from the `init` module alone build it from `node` now.

## Signs the decision was wrong

- A unit needs what another unit owns and `lib/` is the only way: `lib/` grows into the `common/` the structure refuses. The answer is a new unit boundary, not a looser check.
- The check passes a tree that has an import across units: it reads the files by `go.mod`, so a Go file outside any module is the gap, and the check should read it.
- hostd as PID 1 is wanted and the supervision of the other services is easier from it: decision 3 is superseded.
- The host's and the node's images diverge: the image is split by unit, and decision 5 is superseded.
