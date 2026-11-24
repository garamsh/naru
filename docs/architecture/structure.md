# Structure

The shape naru is partitioned into, what may reference what, and when a new unit is earned.

## The shape

Unit, module, Node and host are used as the [glossary](./glossary.md) defines them. naru is divided by responsibility into units, one top-level folder per unit. A unit is a folder of Go modules, usually one, with its binaries under `cmd/`. `lib/` is the one folder that is not a unit: its modules are libraries the units link. Each folder is named for what it runs or publishes, never for a technical role.

Nothing here states what goes inside a unit, or how the units are deployed. A unit's own files follow `docs/convention/stack-go.md` §1; deployment is a separate decision.

## Units

The repository holds these top-level folders (garamsh/discussions Discussion #3 decisions 3, 4 and 6, and [ADR 0039](./adr/0039-init-dissolves-into-lib-init-called-by-host-and-node-and-controlplane-into-lib-vip-and-node-and-a-check-fails-an-import-between-units.md)). `host/`, `node/`, `orch/`, `naructl/`, `tooling/` and `lib/` exist.

- **`host/`**, the host: `naru-hostd`, `naru-init` and `naru-signer` (which, with `naru-orchd`, a Node runs from the `naru-orch` system extension, [ADR 0055](./adr/0055-orch-and-the-signer-are-delivered-as-a-system-extension-a-squashfs-image-init-mounts-read-only.md)).
  - `naru-hostd` is the host API that creates, shows and destroys microVMs on a host through mvmd's runtime API. A service the host's configuration starts when it has `spec.host`, built into the image; it builds an OS-form microVM's NoCloud seed with `lib/iso9660` ([ADR 0028](./adr/0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md)).
  - `naru-signer` is the one process that holds the authorities' keys (`host/cmd/naru-signer`, `host/internal/signer`; [signer.md](./signer.md)).
  - `host/cmd/naru-init` is the host's PID 1, a call of `lib/init/run`. hostd does not become PID 1 yet: it stays a service init supervises, which keeps it restartable on its own (ADR 0039).
- **`node/`**, the Node image's unit, the OS a microVM boots: `node/cmd/naru-init`, the Node's PID 1, a call of `lib/init/run`.
- **An installer**, a binary; its unit is not decided here.
- **`orch/`**, the orchestrator across hosts: `naru-orchd` ([orch.md](./orch.md)). It reaches the hosts and the operator's tool through `lib/orchapi` and `lib/identity`.
- **`naructl/`**, a binary: the operator's tool, which generates the authorities, each Node's user-data and the client files, sends an upgrade bundle to a Node, and calls the Node layout, which is `lib/layout`. Its packages are internal; `tooling/qemu` runs the binary and imports none of them ([ADR 0015](./adr/0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md)).
- **`tooling/`**, the QEMU tests' module, the probe, the disk builder and the import check, each a module of one folder. They are one unit: a module of `tooling/` may import another's public packages.
- **`lib/`**, library modules, one per concern, which no unit's binary is:
  - `lib/init`: what both PID 1s run. Its packages are public because `host/` and `node/` import them: supervision, the state document and its apply, the store, the console, PKI, upgrade, boot, Linux setup, network configuration, time, registry and the filesystem tools. `lib/init/run` is the body of PID 1, `Main()`, and the Node API is `lib/init/api`: there is no separate API server unit or process ([ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)).
  - `lib/iso9660`: the writer of the small ISO 9660 volume a NoCloud drive is, linked by `host/`, `naructl/` and `tooling/qemu` ([ADR 0035](./adr/0035-the-host-api-creates-a-microvm-of-an-os-form-from-a-boot-set-with-a-data-disk-and-a-nocloud-seed-and-the-iso-9660-writer-is-a-library-module.md)).
  - `lib/orchapi`, the types and Go client of `naru-orchd`'s API, and `lib/identity`, the roles, the admission statement, the revocation list and the encrypted files of naru's authorities, which `host/`, `naructl/` and `orch/` link.
  - `lib/wire`: the constants more than one unit shares, defined once: the data disk's label and mount point, the ports, the ESP's GUID, the enrollment key's shape and an exposed port's protocol ([ADR 0070](./adr/0070-shared-constants-are-defined-once-in-the-leaf-module-lib-wire-one-rule-says-when-a-disk-is-blank-and-a-test-parses-what-the-layout-writes.md)).
  - `lib/layout`: the Node layout, the one writer of a Node's user-data (services and their arguments, `spec.host`, the files, mvmd's configuration, the data disk's paths), with the certificate code it calls in `lib/layout/pki`. `naructl` calls it, and orch calls it to build a joining host's configuration ([ADR 0060](./adr/0060-the-gaon-node-layout-is-the-library-lib-layout-called-by-naructl-and-by-orch-when-it-builds-a-joining-hosts-configuration.md)).
  - `lib/hostapi`: the host API's request and response types and its Go client, which `host/` serves and `orch/`, `naructl/` and `tooling/qemu` call ([ADR 0043](./adr/0043-the-host-api-snapshots-and-resumes-a-microvm-gives-it-a-fence-of-its-own-and-its-types-and-client-are-the-module-lib-hostapi.md)); its package `hostapitest` is the conformance test a host API implementation runs ([ADR 0107](./adr/0107-an-exposed-ports-protocol-is-one-type-spelled-tcp-or-udp-in-lib-wire-that-orchs-api-the-host-api-and-hostd-share-amending-adr-0085-items-4-and-6.md)).
  - `lib/machineapi`: the Machine API's types and Go client, `machines/v1alpha1`, which `orch/` is to serve (#381) and garam requires as an external module, with its conformance test `machineapitest` and its in-memory implementation `machineapifake`: a family of three packages, as ADR 0108 item 2 names them ([machines.md](./machines.md) §The API).
  - `lib/atomicfile`: the one write that replaces a file whole: a temporary file, fsync, rename and fsync of the directory, linked by `host/`, `node/`, `orch/`, `naructl/`, `tooling/qemu` and `lib/init`.
  - `lib/metrics`: a daemon's Prometheus registry and the listener that serves it, linked by every daemon that serves `/metrics` on its `--metrics-addr`.
- **Non-Go build inputs**: kernel configuration for the host and guest kernels, and the image definition. They live in `image/`, below. `image/` is not split by unit yet; ADR 0039 defers that.

The rules for units:

- **A top-level folder is one responsibility**: a unit, or a supporting responsibility (`lib/`, `image/`, `docs/`).
- **Units never import each other.** A module of `host/`, `node/`, `orch/`, `naructl/` or `tooling/` imports no package of another top-level folder's modules, only `lib/` and external modules, and a module of `lib/` imports no unit. `make lint-imports` checks it, and also fails when a module under the root is missing from `go.work`'s `use` list ([ADR 0039](./adr/0039-init-dissolves-into-lib-init-called-by-host-and-node-and-controlplane-into-lib-vip-and-node-and-a-check-fails-an-import-between-units.md)).
- **One Go module per executable unit**, its binaries under `cmd/<binary>/main.go`; `tooling/` holds several, one per folder. A binary of one unit may be run by another as a program.
- **A `go.work` at the root names every Go module**, and the root holds no Go package.
- **The modules agree.** `make lint-modules` fails when two modules require one dependency at different versions (indirect requirements included) or declare a `go` directive other than `go.work`'s, and `make build` builds each module with `GOWORK=off`, so a missing `replace` or a requirement only the workspace satisfies fails there.
- **CI runs `make ci` on every pull request and push** (`.github/workflows/ci.yml`): `lint` (`go vet` with and without the `qemu_real` tag, shellcheck over `image/*.sh`, govulncheck over every module), `lint-imports`, `lint-modules`, `format`, `test` and `build`. It runs no QEMU suite: `make test-qemu`, `make test-fleet` and `make test-release-upgrade` print their pass and skip counts, and fail on a skip that `SKIP_ALLOW` in the Makefile does not name. No golangci-lint is configured, and the repository holds no `//nolint` directive.
- **A library linked by more than one unit, or required by another project, is a module of its own under `lib/`.** `lib/` holds no binary. There is no `shared/`, `utils/` or `common/`, and a package that one unit alone imports belongs to that unit. A module that requires a `lib/` module carries a `replace` to its folder, so builds with `GOWORK=off` resolve it.
- **A `lib/` module's packages are public.** The module's own folder holds one package, as `docs/convention/stack-go.md` §1 lays out a module that ships no binary, except where the module is a family of packages that belong together, as `lib/init` is (ADR 0039).
- **A unit's folder is created by its first pull request**, and not before.
- **Non-Go build inputs live in one top-level folder, `image/`**: `image/pkgs/` holds the packages the image is built from, `image/host/disk/` the disk's GUIDs and script, and `image/builder/` the builder, with `image/build.env` and `image/repro.sh` beside them ([ADR 0054](./adr/0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)). It holds no Go module, so it stays outside the units above and outside `go.work`.

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
- [ADR 0039](./adr/0039-init-dissolves-into-lib-init-called-by-host-and-node-and-controlplane-into-lib-vip-and-node-and-a-check-fails-an-import-between-units.md): the units are `host/`, `node/`, `orch/`, `naructl/` and `tooling/` over `lib/`; `init/` is `lib/init`; a check fails an import between units.
- [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md): the Node API is a package of init, now `lib/init/api`, not a unit of its own.
- [ADR 0015](./adr/0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md): `naructl/` is a unit, and `tooling/qemu` reaches it only as a binary.
- [ADR 0028](./adr/0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md): `naru-hostd` is a service binary of `host/`.
- [ADR 0060](./adr/0060-the-gaon-node-layout-is-the-library-lib-layout-called-by-naructl-and-by-orch-when-it-builds-a-joining-hosts-configuration.md): the layout is the module `lib/layout`, called by `naructl` and by orch; it supersedes in part ADR 0015's single owner.
- [ADR 0006](./adr/0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md): the host kernel's source and pin, and `image/` as the folder of the non-Go build inputs.
- [ADR 0114](./adr/0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md): `node/` holds no `naru-ccm`, `naru-volumes`, `naru-vip` or `naru-fence`, and there is no `lib/vip`: naru runs no cluster on its Nodes.

## Open questions

- **How a unit is divided inside.** `docs/convention/stack-go.md` §1 lays out a service module, and no decision here adds to it. Until one does, this binds nothing.
- **`proxy/` (decided, not built).** A unit for `naru-proxy`, the routing proxy that terminates TLS for Machines' stable names and aliases and forwards over the host mesh; a unit because it has its own lifecycle, runs on chosen hosts only, and publishes a surface (the routes file it reads) that stays stable while its inside changes ([ADR 0112](./adr/0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md), [proxy.md](./proxy.md)). Its folder is created by its first pull request.
