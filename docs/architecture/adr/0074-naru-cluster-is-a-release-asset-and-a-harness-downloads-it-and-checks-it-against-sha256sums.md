# ADR 0074: naru-cluster is a release asset, and a harness downloads it and checks it against SHA256SUMS

> Status: superseded by [ADR 0114](./0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md)
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#187, part of goal garamsh/naru#197. It supersedes the install contract of [ADR 0014](./0014-naru-cluster-is-a-public-command-that-boots-nodes-from-a-releases-assets-and-publishes-cluster-json.md) (its Context's second item, Decision item 2's count and its first Consequence); the rest of ADR 0014 stands. Errata on [ADR 0014](./0014-naru-cluster-is-a-public-command-that-boots-nodes-from-a-releases-assets-and-publishes-cluster-json.md), [ADR 0015](./0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md) and [ADR 0019](./0019-the-gaon-topology-and-a-nodes-credential-mode-are-defined-once-in-naructls-layout-and-the-harness-reads-the-topology-from-naructl-topology.md) point here.

## Context

- **ADR 0014 documented `GOBIN=<dir> go install …/naru-cluster@<commit>` and required `tooling/qemu`'s `go.mod` to have no `replace` directive.** `tooling/qemu/go.mod` has seven (`replace` lines for `lib/` modules), added so that the module builds against the workspace's `lib/` modules. Go refuses `go install pkg@version` for a module whose `go.mod` has `replace` directives, so the documented form fails.
- **No test ran the documented form.** The harness test built `naru-cluster` in the workspace, so it exercised a build no harness can make.
- **A release already carries `naructl` as a static binary** built by `make release` in the builder container, with `gobuild` from `image/lib.sh`, and a harness already downloads it.

## Decision

**`naru-cluster` is a release asset, `naru-cluster-<tag>-linux-amd64`. A harness gets it from the release it boots, checks it against the release's `SHA256SUMS` and runs it. [harness.md](../harness.md) states that contract and no other install form.**

1. **The asset is a static binary** (`CGO_ENABLED=0`) built by `make release` inside the builder with `gobuild`, so `require_go` checks the Go release it records, and listed in `SHA256SUMS`. The release has fifteen files, `SHA256SUMS` among them.
2. **The `go install …@<commit>` form is removed** from [harness.md](../harness.md) and from the comments of `naru-cluster`'s `main.go` and of `internal/naructl`.
3. **`make test-harness RELEASE=<dir>` runs the test against the directory's asset.** The test checks the asset against the directory's `SHA256SUMS`, makes it executable as the contract says and runs it; it fails, and does not skip, when the directory has no asset, more than one, or one `SHA256SUMS` does not vouch for. Without `RELEASE`, the target builds the release and the test builds `naru-cluster` in the workspace, as before.
4. **`tooling/qemu` may have `replace` directives.** It still builds with `GOWORK=off`, which the checks run on every module.

## Consequences

- **The release grows by one file**, and the count in `release.yml` and [image.md](../image.md) is fifteen.
- **A release made before this one has no `naru-cluster` asset.** A harness boots such a release with the `naru-cluster` of a later one, which reads the older release's assets as before.
- **The asset is the build of the release's commit**, so a harness that wants a `naru-cluster` for another commit makes a release of it.
- **A change to a flag or to `cluster.json` is still a breaking change of a public surface** ([harness.md](../harness.md)).

## Signs the decision was wrong

- A harness runs on a platform other than linux/amd64, which would add assets by platform.
- Harnesses want `naru-cluster` between releases often enough that a release per change is too slow, which would say the module should build with no `replace` so that `go install` works again.

## Alternatives rejected

- **Removing the `replace` directives.** The module would then pin published versions of `lib/` modules, and every change to a `lib/` module would need a tag before `tooling/qemu` could use it.
- **Building from a checkout of the commit.** It needs the repository and the Go toolchain on the harness's host, which ADR 0014 meant to avoid.
