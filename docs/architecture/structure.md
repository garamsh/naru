# Structure

The shape naru is partitioned into, what may reference what, and when a new unit is earned.

## The shape

naru is divided by responsibility into units, one top-level folder per unit. A unit is a folder of Go modules, usually one, with its binaries under `cmd/`. `lib/` is the one folder that is not a unit: its modules are libraries the units link. Each folder is named for what it runs or publishes, never for a technical role.

Nothing here states what goes inside a unit, or how the units are deployed. A unit's own files follow `docs/convention/stack-go.md` §1; deployment is a separate decision.

## Units

The repository holds these top-level folders (garamsh/discussions Discussion #3 decisions 3, 4 and 6, and [ADR 0039](./adr/0039-init-dissolves-into-lib-init-called-by-host-and-node-and-controlplane-into-lib-vip-and-node-and-a-check-fails-an-import-between-units.md)). `host/`, `node/`, `orch/`, `naructl/`, `tooling/` and `lib/` exist.

- **`host/`**, the host: `naru-hostd`, `naru-init` and `naru-signer`.
  - `naru-hostd` is the host API that creates, shows and destroys microVMs on a host through mvmd's runtime API. A service the host's configuration starts when it has `spec.host`, built into the image; it builds an OS-form microVM's NoCloud seed with `lib/iso9660` ([ADR 0028](./adr/0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md)).
  - `naru-signer` is the one process that holds the authorities' keys (`host/cmd/naru-signer`, `host/internal/signer`; [signer.md](./signer.md)).
  - `host/cmd/naru-init` is the host's PID 1, a call of `lib/init/run`. hostd does not become PID 1 yet: it stays a service init supervises, which keeps it restartable on its own (ADR 0039).
- **`node/`**, the node, the OS a microVM boots:
  - `node/cmd/naru-init`, the node's PID 1, a call of `lib/init/run`;
  - `naru-vip`, which holds the control plane's virtual address while its node is the first member whose API server answers an HTTPS request ([ADR 0036](./adr/0036-a-re-print-carries-at-most-the-newest-16-kib-of-the-console-log-and-the-api-servers-liveness-probe-is-a-tls-request-that-it-can-answer.md));
  - `naru-fence`, which closes etcd's ports to addresses outside the control plane.

  `naru-vip` and `naru-fence` are services the node's configuration starts, built into the image; init holds no election ([ADR 0024](./adr/0024-the-control-plane-is-three-etcd-members-with-tls-on-both-ports-and-a-virtual-address-held-by-naru-vip-behind-a-fence-on-etcds-ports.md)). The fence's rules are `node/internal/fence`; the VIP's election and its Linux host are `lib/vip`.
- **An installer**, a binary; its unit is not decided here.
- **`orch/`**, the orchestrator across hosts: `naru-orchd` ([orch.md](./orch.md)). It reaches the hosts and the operator's tool through `lib/orchapi` and `lib/identity`.
- **`naructl/`**, a binary: the operator's tool, which generates a cluster CA, each node's user-data and the operator's client certificates, sends an upgrade bundle to a node, and is the single owner of the gaon node layout. Its packages are internal; `tooling/qemu` runs the binary and imports none of them ([ADR 0015](./adr/0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md), [ADR 0019](./adr/0019-the-gaon-topology-and-a-nodes-credential-mode-are-defined-once-in-naructls-layout-and-the-harness-reads-the-topology-from-naructl-topology.md)).
- **`tooling/`**, the QEMU tests' module, the probe, the disk builder and the import check, each a module of one folder. They are one unit: a module of `tooling/` may import another's public packages.
- **`lib/`**, library modules, one per concern, which no unit's binary is:
  - `lib/init`: what both PID 1s run. Its packages are public because `host/` and `node/` import them: supervision, the state document and its apply, the store, the console, PKI, upgrade, boot, Linux setup, network configuration, time, registry and the filesystem tools. `lib/init/run` is the body of PID 1, `Main()`, and the Node API is `lib/init/api`: there is no separate API server unit or process ([ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)).
  - `lib/vip`: the virtual address's election, linked by `node/` now and by `orch/` for its own address.
  - `lib/iso9660`: the writer of the small ISO 9660 volume a NoCloud drive is, linked by `host` and by `tooling/qemu` ([ADR 0035](./adr/0035-the-host-api-creates-a-microvm-of-an-os-form-from-a-boot-set-with-a-data-disk-and-a-nocloud-seed-and-the-iso-9660-writer-is-a-library-module.md)).
  - `lib/orchapi`, the types and Go client of `naru-orchd`'s API, and `lib/identity`, the roles, the admission statement, the revocation list and the encrypted files of naru's authorities, which `host/`, `naructl/` and `orch/` link.
  - `lib/hostapi`: the host API's request and response types and its Go client, which `host/` serves and `orch/` calls ([ADR 0043](./adr/0043-the-host-api-snapshots-and-resumes-a-microvm-gives-it-a-fence-of-its-own-and-its-types-and-client-are-the-module-lib-hostapi.md)).
  - `lib/metrics`: a daemon's Prometheus registry and the listener that serves it, linked by every daemon that serves `/metrics` on its `--metrics-addr`.
  - A node API client, which gaon requires as an external module (Discussion #3 decision 5).
- **Non-Go build inputs**: kernel configuration for the host and guest kernels, and the image definition. They live in `image/`, below. `image/` is not split by unit yet; ADR 0039 defers that.

The rules for units:

- **A top-level folder is one responsibility**: a unit, or a supporting responsibility (`lib/`, `image/`, `docs/`).
- **Units never import each other.** A module of `host/`, `node/`, `orch/`, `naructl/` or `tooling/` imports no package of another top-level folder's modules, only `lib/` and external modules, and a module of `lib/` imports no unit. `make lint-imports` checks it and `.github/workflows/ci.yml` runs it on every pull request and push ([ADR 0039](./adr/0039-init-dissolves-into-lib-init-called-by-host-and-node-and-controlplane-into-lib-vip-and-node-and-a-check-fails-an-import-between-units.md)).
- **One Go module per executable unit**, its binaries under `cmd/<binary>/main.go`; `tooling/` holds several, one per folder. A binary of one unit may be run by another as a program.
- **A `go.work` at the root names every Go module**, and the root holds no Go package.
- **A library linked by more than one unit, or required by another project, is a module of its own under `lib/`.** `lib/` holds no binary. There is no `shared/`, `utils/` or `common/`, and a package that one unit alone imports belongs to that unit. A module that requires a `lib/` module carries a `replace` to its folder, so builds with `GOWORK=off` resolve it.
- **A `lib/` module's packages are public.** The module's own folder holds one package, as `docs/convention/stack-go.md` §1 lays out a module that ships no binary, except where the module is a family of packages that belong together, as `lib/init` is (ADR 0039).
- **The node API client gaon requires is a module under `lib/`**, as garamsh/mvmd's `lib/runtimeapi` is for gaon. gaon requires it as an external module and never copies it in.
- **A unit's folder is created by its first pull request**, and not before.
- **Non-Go build inputs live in one top-level folder, `image/`**: `image/host/` holds what the host image alone builds, `image/node/` what the Node image alone builds, and `image/common/` what both carry, with `image/build.env` the one file beside them ([ADR 0054](./adr/0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)). It holds no Go module, so it stays outside the units above and outside `go.work`.
- **Dependencies between projects are one-way**: gaon calls naru's node API, and naru does not know gaon's resource API.

## Boundaries and dependency direction

- **A unit is the boundary.** One folder per unit, named for what it runs or publishes and never for a technical role.
- **A unit is reached only through what it declares public.** Everything else it holds is internal, and reaching past that surface is a defect even where the language permits it.
- **A unit reaches another only as a program, or through `lib/`.** It imports no package of another unit, public or internal, and `make lint-imports` fails the build where it does. This is the side taken on whether a boundary may depend on a sibling, which the domain structure leaves contested ([ADR 0001](./adr/0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md)); ADR 0039 narrows it from "only what the unit publishes" to "nothing".
- **Shared code sits below every unit, never beside one.** What two units both need is extracted into `lib/`, or owned by one unit and reached through what it publishes; it never becomes a sibling folder no unit owns.
- **A cycle between units means the split is wrong**, and so does a file that belongs in two of them.
- **A new unit is earned** by a responsibility with its own lifecycle and vocabulary, which something outside it depends on, and whose published surface can stay stable while its inside changes.

## Where the composition site lives

- **The application has one composition site.** It is the one place that constructs concrete implementations, and it decides which implementation each declared abstraction receives.
- **Every other unit receives what it depends on and constructs none of it**, whatever the shape makes the unit: a domain, a layer, or an adapter.
- **The composition site stays thin.** It constructs and passes; it decides nothing else.

Each executable unit has its own site: its `cmd/<binary>/main.go`, or the package that file calls where a test outside `cmd/<binary>/` has to reach it (`docs/convention/stack-go.md` §Project envelope).

## Decisions this rests on

- [ADR 0001](./adr/0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md): the repository is a Go workspace of executable units, each its own module, partitioned by responsibility.
- [ADR 0039](./adr/0039-init-dissolves-into-lib-init-called-by-host-and-node-and-controlplane-into-lib-vip-and-node-and-a-check-fails-an-import-between-units.md): the units are `host/`, `node/`, `orch/`, `naructl/` and `tooling/` over `lib/`; `init/` is `lib/init` and `controlplane/` is `lib/vip` and `node/`; a check fails an import between units.
- [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md): the Node API is a package of init, now `lib/init/api`, not a unit of its own.
- [ADR 0015](./adr/0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md): `naructl/` is a unit, and `tooling/qemu` reaches it only as a binary.
- [ADR 0024](./adr/0024-the-control-plane-is-three-etcd-members-with-tls-on-both-ports-and-a-virtual-address-held-by-naru-vip-behind-a-fence-on-etcds-ports.md): `naru-vip` and `naru-fence` are service binaries; they moved to `node/` and `lib/vip` in ADR 0039.
- [ADR 0028](./adr/0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md): `naru-hostd` is a service binary of `host/`.
- [ADR 0019](./adr/0019-the-gaon-topology-and-a-nodes-credential-mode-are-defined-once-in-naructls-layout-and-the-harness-reads-the-topology-from-naructl-topology.md): the gaon topology lives in `naructl`'s layout, and `tooling/qemu` gets it from `naructl topology`, not by import.
- [ADR 0006](./adr/0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md): the host kernel's source and pin, and `image/` as the folder of the non-Go build inputs.

## Open questions

- **How a unit is divided inside.** `docs/convention/stack-go.md` §1 lays out a service module, and no decision here adds to it. Until one does, this binds nothing.
