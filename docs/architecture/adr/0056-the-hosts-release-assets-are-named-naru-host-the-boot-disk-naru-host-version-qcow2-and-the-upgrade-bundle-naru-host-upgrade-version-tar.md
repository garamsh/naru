# ADR 0056: the host's release assets are named naru-host, the boot disk `naru-host-<version>.qcow2` and the upgrade bundle `naru-host-upgrade-<version>.tar`

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#113 (decision 4); it serves the criterion of garamsh/aurora#811 that a release ships both `naru-host` and `naru-node`. It changes the names ADR 0014 (the boot disk) and ADR 0020 (the upgrade bundle) gave; both get an appended erratum and are not otherwise edited.

## Context

- **The release has two artifacts and one of them has no name.** Plan #113 names `naru-host` and `naru-node`. The Node boot set ships as `naru-node-<tag>...` (ADR 0046), but the host's boot disk was `naru-<tag>.qcow2` and its upgrade bundle `naru-upgrade-<tag>.tar`, the names from before the split into host and node. A reader of the release finds no `naru-host`.
- **Three readers depend on the names.** `make release` writes them, the release workflow reads the previous release's bundle to check the room of the trial slot (ADR 0037), and `naru-cluster` reads the boot disk of a release directory (ADR 0014). Releases up to `v0.1.0-dev.8` carry the old names and stay published.

## Decision

1. **The boot disk is `naru-host-<version>.qcow2` and the upgrade bundle is `naru-host-upgrade-<version>.tar`.** Every other asset keeps its name, `naru-data-<version>.qcow2` included, and the release still has fourteen files.
2. **The release workflow reads either name of the previous release's bundle.** It looks for `naru-host-upgrade-*.tar` first and takes `naru-upgrade-*.tar` when the previous release has no file under the new name. A tag with no earlier release still skips the check and says so.
3. **`naru-cluster` reads `naru-host-<version>.qcow2` and falls back to `naru-<version>.qcow2`** when the release directory has no file under the new name, so a harness pointed at an older release still boots it. When both exist the new name is the one read. A directory with neither is refused, naming both.
4. **`naructl upgrade` is unchanged by this decision.** It takes the bundle by the path it is given and finds its line in `SHA256SUMS` by the file's name, so it needs no knowledge of either name.

## Rejected alternatives

- **Leave the names and describe the host artifact in the release notes.** The criterion and the plan name the artifact itself, and a reader listing the assets must find it; notes can be edited away from the files they describe.
- **Publish both names for a time.** It doubles the largest asset and the file count the workflow asserts, for a reader who can be served by a fallback in the one command that reads old releases.
- **Rename `naru-data-<version>.qcow2` as well.** The data disk is shared by both artifacts' tests; it belongs to neither.

## Consequences

- A release after `v0.1.0-dev.8` has no `naru-<version>.qcow2` and no `naru-upgrade-<version>.tar`. A script that downloads those by name must change.
- The fallback in `naru-cluster` and the second pattern in the workflow are removed together once no published release carries the old names that anyone still boots or upgrades from.

## Signs the decision was wrong

- A release is published whose assets are found by an old name often enough that the fallback becomes the usual path.
- A third reader needs the same two-name lookup, which would say the lookup belongs in one place both can import.
