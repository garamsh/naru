# ADR 0019: the gaon topology and a Node's credential mode are defined once in naructl's layout, and the harness reads the topology from `naructl topology`

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#45, part of goal garamsh/naru#44. It adds to [ADR 0015](./0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md) and [ADR 0016](./0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md) and changes neither decision. ADR 0015 made `naructl` the single owner of the gaon Node layout, and the service list was in fact written in four places.

## Context

- **The service list, the control-plane membership and the signer rule each had several definitions.** `naructl/internal/layout` named the services and marked the control-plane ones; `naru-cluster` kept its own list of names for `-disable` and for what to wait for on each Node, and its own copy of "`gaon-controllers` cannot be disabled when the Node signs"; the judge kept its list of what init starts and skips. A service added or renamed in the layout left the harness and the judge expecting the old names, and nothing failed until a boot did.
- **A Node's credential mode was two booleans.** `ControlPlane` and `PairsAsFiles` together gave four modes, and the code branched on the pair in several places.
- **`tooling/qemu` cannot import the layout.** It builds with `GOWORK=off` and no `replace` ([ADR 0014](./0014-naru-cluster-is-a-public-command-that-boots-nodes-from-a-releases-assets-and-publishes-cluster-json.md)), and it runs `naructl` as a binary ([ADR 0015](./0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md)).

## Decision

1. **The layout defines the topology once.** `layout.Gaon` is the ordered list of services, each with whether only the control-plane Node runs it and whether it is one of gaon's binaries, and names the signer (the service that signs the agents' requests), the API server and the Node Agent. User-data, `naructl`'s refusals and everything below derive from it.
2. **`naructl topology` prints it as JSON.** The harness runs the `naructl` it already runs (the release's asset in `naru-cluster`, a build of the checkout in the QEMU tests) and decodes the output. `naru-cluster` takes its service names, which services run on which Node, what it waits for, which names `-disable` accepts and the signer rule from it. The judge takes the services to expect from it, through a new `-naructl` flag that `-gaon` requires.
3. **The credential mode is one type.** `layout.Mode` has one value for each of the four modes (worker or control plane, token or files). The one branch on it is `plan` in the layout, which decides what a Node carries; the rest of the layout asks the mode a question (`ControlPlane`, `PairsAsFiles`, `Signs`) and does not compare values.
4. **A `naructl` that has no `topology` command is refused**, by `naru-cluster` and by the judge, with an error that says it has none. The harness has no copy of the list to fall back on, because a fallback copy is the second definition this decision removes. Releases `v0.3.0-rc.1` and `v0.3.0` ship such a `naructl`; the next release does not.
5. **`naructl`'s command line, its outputs and `naru-cluster`'s flags are unchanged.** The only addition is the `topology` command, and the judge's `-naructl` flag.

## Consequences

- **A service added or renamed in the layout is seen by the harness and the judge with no change to either.** A test in the layout compares the topology with the services the user-data of every mode runs, and a test in `tooling/qemu/internal/naructl` runs the `naructl` built from the checkout and compares its topology with that user-data.
- **A release must carry a `naructl` with `topology`** to be booted by `naru-cluster` or judged with `-gaon`. A harness pinned to an older release needs a newer one.
- **`naru-cluster` validates `-disable` after it has read the release's `naructl`**, since the names come from it. A bad name is still refused before any disk is converted or Node started; the staged `naructl` asset is the only work done before the refusal.
- **`GaonIssued`, the judge's list of certificate names init issues from `spec.pki`, stays in the judge.** They are certificate names and not services (the agent has none, ADR 0016), and `naructl`'s layout does not export them.

## Signs the decision was wrong

- Another consumer of the layout appears that cannot run a binary, which would say the topology belongs in a package both modules may import.
- The topology needs a field the harness reads that is not about services, which would say `naructl topology` is becoming a second `cluster.json` and wants a versioned contract.
- A pre-`topology` release has to be booted again, which would say the refusal in item 4 should give way to a pinned fallback.
