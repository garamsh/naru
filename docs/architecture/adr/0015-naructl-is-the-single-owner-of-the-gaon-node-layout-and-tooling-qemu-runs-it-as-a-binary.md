# ADR 0015: naructl is the single owner of the gaon Node layout, and tooling/qemu runs it as a binary

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#36, part of goal garamsh/naru#34, with the PM's amendment of that issue. It adds to [ADR 0012](./0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md), [ADR 0013](./0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md) and [ADR 0014](./0014-naru-cluster-is-a-public-command-that-boots-nodes-from-a-releases-assets-and-publishes-cluster-json.md), and changes two items of them: it amends [ADR 0014](./0014-naru-cluster-is-a-public-command-that-boots-nodes-from-a-releases-assets-and-publishes-cluster-json.md) item 1, which lists the assets `-release` reads, by adding `naructl-<version>-linux-amd64` to them and the workflow's count (ADR 0014 item 2: seven files) to eight; and it supersedes the test CA of [ADR 0012](./0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md)'s Consequences, "valid for 48 hours", with a CA of ten years and certificates of one year (Consequences below). The rest of both stands.

## Context

- **An operator has no tool for a gaon cluster.** The user-data of a gaon Node, with its services, certificates and mvmd's configuration, was written only by test code in `tooling/qemu` (`internal/node`, `internal/pki`), and the release's `.test.yaml` files came from the same code. A deployer wrote the files by hand.
- **A second writer of the layout drifts.** If an operator's tool and the tests each wrote the layout, a change to a service's flags or to a path would be made in two places, and only the tests would notice when one was missed.
- **`tooling/qemu` builds with `GOWORK=off` and no `replace`** ([ADR 0014](./0014-naru-cluster-is-a-public-command-that-boots-nodes-from-a-releases-assets-and-publishes-cluster-json.md): `go install …@<commit>` must work). It cannot import a sibling module of the workspace.
- **What QEMU needs and an operator may also want is the same set of inputs:** extra serving addresses, per-service binary paths, a service left out, and a second NIC.

## Decision

**`naructl` is a new unit, `naructl/`, a module with its binary under `cmd/naructl/` ([ADR 0001](./0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md)). It generates a cluster CA, each Node's user-data and `network-config`, and operators' client certificates, and it is the single owner of the gaon Node layout. `tooling/qemu` runs the binary and imports none of its packages.**

1. **One owner of the layout.** The gaon services and their arguments, `spec.pki` on the control-plane Node, the worker's files, mvmd's configuration and the data disk's paths are written in `naructl/internal/layout` and nowhere else. `tooling/qemu` keeps only the Nodes that are not gaon's (the stand-ins and the probe) and what QEMU itself is: the NICs, the disks and the processes.
2. **The surface is the command line.** `naructl gen ca`, `gen client` and `gen node`; what QEMU needs beyond an operator's defaults is a flag (`--serving-address`, `--binary <service>=<path>`, `--disable`, `--extra-nic`, `--pairs-as-files`), not post-processing of the output. Each file `gen node` writes is one `naru-init -validate` accepts, and the tests run that validation with init built from its own module.
3. **It ships as a static binary**, the release asset `naructl-<version>-linux-amd64`, listed in `SHA256SUMS`. `make release` builds it and runs it to write the test user-data from a CA that lives only for the run, which node-1's file carries as certificates and no CA key. The QEMU tests build it from the checkout, and `naru-cluster` runs the release's copy, so the layout the harness boots is the layout the release names.
4. **The CA's key goes only into the control-plane Node's user-data and the operator's secret store**, and into no release asset.
5. **Public surfaces keep their form.** `naru-cluster`'s flags, `cluster.json` and `version` do not change; the release gains an asset, so its workflow checks eight files, and `-release` reads one more.

## Consequences

- **A change to a gaon service's flags or paths is made once**, in `naructl`, and the QEMU tests, `naru-cluster` and the release all pick it up.
- **The tests build `naructl` and run it**, so the QEMU tests and the release tests need the Go toolchain and the checkout; `naru-cluster` needs neither, only the asset.
- **`naru-cluster` refuses every release without a `naructl-<version>-linux-amd64`**, naming the asset, as ADR 0014 did for `naru-inputs-<version>.json`. Every release made before this one is such a release; a harness that pins an older release must move to one that carries the asset, and [harness.md](../harness.md) says so.
- **Test certificates now last as long as `naructl`'s** (a CA of ten years, certificates of one year) and no longer 48 hours, because the operator's and the test tool's certificates are one code path. A cluster of tests still lives for its run.
- **The guest manifest's reader is written twice**: `tooling/qemu/internal/guest` for the data disk's tree, and `naructl`'s own, because `naructl` may not import `tooling/qemu`. They read one file format; the duplication is the price of the boundary.
- **The Nodes that are not gaon's keep a writer of mvmd's configuration and `network-config` in `tooling/qemu/internal/node`.** The stand-ins and the probe run on image components that no operator deploys.
- **QEMU names a few of the layout's constants** (the API port, the data disk's paths, the directory its binaries are copied to) and the `/24` prefix it derives from its netmask. A rename of one in `naructl` is found by the QEMU tests failing, not by a compiler.

## Signs the decision was wrong

- Each change of the layout still needs a matching change under `tooling/qemu`, which would say the constants QEMU names belong in a flag or in `naructl`'s output.
- An operator needs a form of Node that the flags cannot express and files a request for post-processing, which would say the flag set is the wrong surface.
- `naru-cluster` boots a layout from a release's `naructl` that its own harness-supplied binaries do not match, which would say the layout needs a version.
