# ADR 0014: naru-cluster is a public command that boots Nodes from a release's assets and publishes cluster.json

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#38, part of goal garamsh/naru#34. It adds to [ADR 0012](./0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md) and [ADR 0013](./0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md) and supersedes no item of either.

## Context

- **gaon's harness (garamsh/gaon#9) tests gaon on naru's Nodes and lives in gaon's repository.** It has no checkout of naru. [ADR 0001](./0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md) lets a unit be reached only through what it publishes, and the QEMU tests' tooling in `tooling/qemu` publishes nothing: its tests build their inputs from the repository root with `go build` and read the image's build directory.
- **A Go module can be run by commit.** `go run <module>/cmd/<command>@<commit>` fetches the module's source and nothing else: no repository root, no `build/`, no sibling module.
- **Everything a Node needs besides the boot disk is already in the release**, except two facts the tests read from `build/image/inputs.env` and from the test image: the Firecracker digest that mvmd's configuration names, and the reference of the test image.
- **The harness supplies the gaon binaries.** It tests the commit it is on, not the pin the image carries.

## Decision

**`tooling/qemu` publishes one command, `naru-cluster`, which boots Nodes from a release directory and the gaon binaries a harness names, writes `cluster.json` once every Node is up, and runs until it is signalled. The command, its flags and `cluster.json` are a public contract, specified in [harness.md](../harness.md).**

1. **The command reads a release, nothing of a checkout.** Its inputs are the release directory (`-release`), flags, and what the module compiles in. It takes the boot disk from `naru-<version>.qcow2`, the inputs tree (the registry, its TLS authority, the test image) from the data disk `naru-data-<version>.qcow2`, the guest manifest from `naru-guest-<version>.json`, and the Firecracker digest and the test image's reference from a new asset, `naru-inputs-<version>.json`. It builds no program: the registry is on the data disk.
2. **The release gains one asset, `naru-inputs-<version>.json`.** It is written by `make release` from what the tests stage, listed in `SHA256SUMS`, and counted by the release workflow, which now checks seven files.
3. **The Nodes are the QEMU tests' Nodes.** The command calls the same `internal/node` code the tests call; a Node's disks, NoCloud drive, network and process are made in one place.
4. **Harness-supplied gaon binaries replace the image's by path.** A binary found in `-gaon-bin` is copied to the data disk under `inputs/gaon/` and its service's configuration names that path; a binary not found leaves the image's.
5. **`-disable` removes a service from every Node's configuration.** It is the only way to leave a service out; an absent binary does not.
6. **`cluster.json` is the one output**, written last and atomically, with a `version` that changes only when a documented field is removed, renamed or changes meaning. A field added is optional and does not change it.
7. **A Node that is killed is lost, not failed.** The command logs it, does not restart it and keeps the others; a harness's `kill -KILL` is a power-off.
8. **The module's internals stay internal.** Only the command and its output are public; the packages under `internal/` are not, and a harness does not import them.

## Consequences

- **A change to a flag, to `cluster.json` or to what a harness may rely on is a breaking change of a public surface** and is made as one: a new `version`, or a new flag, by the pull request that changes [harness.md](../harness.md).
- **`go run …@<commit>` needs the module to build with no `replace` directive and no workspace.** `tooling/qemu`'s `go.mod` has none, and `GOWORK=off go build ./...` is part of the checks.
- **A release made before this one has no `naru-inputs-<version>.json`** and `naru-cluster` refuses it, naming the asset.
- **The data disk's tree is read with `debugfs` and the disks with `qemu-img`**, which the tests need already; a harness host needs `/sbin/mkfs.ext4`, `/sbin/debugfs`, `/sbin/e2fsck`, `qemu-img`, `qemu-system-x86_64`, the OVMF files and `/dev/kvm`.
- **The Machine test's image is the image of every cluster**, so a harness that needs another image brings it to the Nodes itself.

## Signs the decision was wrong

- A harness needs a field `cluster.json` lacks often enough that adding fields is routine, which says the file should be derived from the Nodes and not stated by hand.
- The release's inputs asset drifts from what the tests stage, which would say the tests should read the release too.
