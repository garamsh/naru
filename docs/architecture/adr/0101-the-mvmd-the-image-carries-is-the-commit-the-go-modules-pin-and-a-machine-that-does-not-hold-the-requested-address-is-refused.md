# ADR 0101: the mvmd the image carries is the commit the Go modules pin, and a Machine that does not hold the requested address is refused

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#262. It supersedes the part of [ADR 0043](./0043-the-host-api-snapshots-and-resumes-a-microvm-gives-it-a-fence-of-its-own-and-its-types-and-client-are-the-module-lib-hostapi.md) item 6 that has the image's mvmd pin move at the end of a round, and the place [ADR 0003](./0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md) and [ADR 0010](./0010-the-guest-kernel-and-initrds-are-image-components-built-at-the-pinned-mvmd-commit.md) give the pinned commit (`image/pkgs/mvmd/pin.env`); the rest of those decisions stands. It extends [ADR 0099](./0099-a-group-template-lists-its-members-addresses-by-number-orch-places-a-member-on-a-host-whose-machine-prefix-holds-its-address-and-mvmd-gives-it.md) item 5.

## Context

- **The commit was written twice.** `host/go.mod` (and the two tooling modules) pinned mvmd's `lib/runtimeapi` at `b0a6fbe45116301a1a39af97006c7d4a33d8849f`, which carries `requested_ipv4` (garamsh/mvmd#817), and the image built mvmd from `MVMD_COMMIT` in `image/pkgs/mvmd/pin.env`, `b1a60cd649673a6db5fa72ca8cc23f088fd83639`, thirteen commits earlier. The pins had been moved apart on purpose (ADR 0043 item 6) and nothing made them meet again.
- **An old daemon does not refuse the new field.** It ignores it, so a create that asked for `10.200.2.10` returned a Machine at `10.200.2.2`, and the Node's certificates, signed before the Node existed, named the other address.
- **hostd took whatever address came back.** Only the repeat of a request compared the Machine's addresses with the one requested.

## Decision

1. **The image's mvmd commit is the one `host/go.mod` pins.** `image/pkgs/mvmd/pin.env` names the repository and the module (`MVMD_COMMIT_FROM`) and holds no commit. `image/pkgs/mvmd-commit.sh` reads that module's version in `host/go.mod`, has go resolve the pseudo-version to the whole commit hash, and `build.sh` passes it to BuildKit as `MVMD_COMMIT`, which the `mvmd` and `guest-initrds` packages take as a build argument and fail without. The release's component list resolves the same way (`release.CommitOf`), so the SBOM names the commit the image was built from.
2. **The three modules agree or the build fails.** `mvmd-commit.sh` refuses a `tooling/qemu` or `tooling/probe` that pins another version, and a test of `tooling/qemu` does the same.
3. **The digests of the guest initrds remain checked values.** They cannot be derived, so they move in the pull request that moves the pin; a build at a commit whose initrds differ fails and prints the digest it got and the commit.
4. **A Machine that does not hold the requested address is refused.** After mvmd creates the Machine, hostd compares its addresses with `address` of the request; when it is not among them hostd removes what the request made and answers 409 naming both addresses.

## Options considered

- **Keep two pins and a test that compares them.** It states the commit twice and ends at a failing build; resolving it from the Go modules states it once.
- **Pin the image's mvmd by a release or tag.** A tag can be moved; the commit is what `go.sum` fixes.
- **Take the address from the Machine as mvmd gave it, and write that into the seed.** The certificates are signed before the Machine exists.

## Consequences

- Moving the Go modules' pin moves the image's mvmd, and the digests of `image/pkgs/guest-initrds/pin.env` with it; a pull request that moves one and not the other fails to build the image.
- `build.sh` and the release need `go` and the module's source (the module cache or the proxy) to resolve the hash.
- A host with an mvmd older than the Go client now fails a create that asks for an address, where it ran the member at another one before.

## Signs the decision was wrong

- An image that must carry an mvmd the host's client is not compiled against, for a reason this repository cannot state in the Go modules.
