# ADR 0063: the image is built inside one digest-pinned builder container whose toolchains are pinned, and the stage-2 direction is a musl toolchain with BuildKit-built packages

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#178, part of goal garamsh/naru#197. It supersedes item 4 of [ADR 0006](./0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md) ("No containers") and item 3 of [ADR 0011](./0011-mkfs-ext4-is-a-static-mke2fs-built-from-the-pinned-e2fsprogs-release-and-installed-in-the-image.md) ("the host's `libc.a` is used") in part, and records the direction of garamsh/naru#189. The facts about the old build are those of `docs/architecture/image.md` before this change; the builder's package versions were read from the Ubuntu snapshot service on 2026-11-15.

## Context

- **The image was built with whatever the machine had.** `make image` used the host's gcc, binutils, glibc `libc.a` and Go. The kernel build records the compiler version, so a workstation and the release runner built other bytes; `image.md` said so in one place ("has not been checked") and claimed the opposite in another ("a second `make release` gives the same boot disk").
- **Go was not one toolchain.** naru's own binaries used the host's `go`; each third-party build chose its own `GOTOOLCHAIN` from its sources.
- **The skip logic and a pin were duplicated.** Fifteen scripts computed the same stamp; util-linux was pinned twice (2.42.4 and 2.42).
- **A glibc warning was ignored.** A static link of `getpwnam` or `getaddrinfo` prints that it needs the shared libraries of the glibc it was linked with; the image has no libc, so the call fails at run time. The build printed it and went on.
- **The release workflow installed the build's tools with apt on a floating runner image** and ran `make release` without `make ci`.

## Decision

**`make image`, `make image-upgrade`, `make disk`, `make release` and `make image-repro` run inside one builder container, built from `image/builder/Dockerfile` and referenced by its image digest. The container carries pinned Go and C toolchains. Two builds of one commit, on two machines, give the same `SHA256SUMS` apart from the assets that differ by design.**

1. **The builder image.** The base is `ubuntu:24.04` by index digest. Every package is installed at an exact version from a dated Ubuntu snapshot (`snapshot.ubuntu.com`), so the version names the bytes. Go 1.26.11 is the release tarball, verified by SHA-256, and `GOTOOLCHAIN` is set to it so no build downloads another. `rustup-init` is verified by SHA-256; it installs the channel mvmd's `rust-toolchain.toml` names. The image is built with a fixed `SOURCE_DATE_EPOCH` and `rewrite-timestamp`, and two `--no-cache` builds gave the same config digest, which `image/builder/builder.env` pins. `image/builder/run.sh` builds the image on a machine that does not have it and fails unless the config digest is the pinned one. The config digest is pinned rather than the manifest digest because it does not depend on how a layer is compressed.
2. **One Go for every binary.** naru's own binaries, mvmd's, and the tools of `tooling/` are built by the builder's Go, and `go version -m` on each shipped binary is checked against it (`require_go`). Third-party builds (etcd, Kubernetes, containerd, the CNI plugins, gaon and the credential helpers) keep the toolchain their sources name, which the builder's Go downloads; they are not asked to equal the builder's.
3. **The container is run by the user.** `run.sh` starts it as the calling user's uid and gid with the repository mounted at `/naru`, over any Docker daemon the user can reach. It needs no root of its own and installs nothing on the machine. Podman is reached by setting `DOCKER=podman`.
4. **A static link that needs glibc fails the build** unless the binary is on `image/nss-allow.txt`, which names the binary and why its call cannot happen.
5. **One stamp function.** `build_current` in `image/lib.sh` replaces the fifteen copies; its stamp includes the builder's name, so another builder rebuilds. util-linux has the one pin of `image/common/util-linux/pin.env`, which xfsprogs sources.
6. **The release workflow runs `make ci` and then `make release`** and records the builder's identity. It no longer installs packages.
7. **Stage 2 (garamsh/naru#189), recorded and not built here.** The container is stage 1 of the Talos model, where the image is built from a package graph. The direction of stage 2 is a musl toolchain, so that the static links stop depending on glibc, and packages built by BuildKit as OCI images, each from a pinned source and a Dockerfile, assembled by `tooling/disk`, which naru keeps. Stage 2 supersedes the builder's package list and the `build.sh` of each component; the builder, its digest and the `SHA256SUMS` comparison stay.

## Consequences

- **The build needs a Docker (or Podman) daemon the user can reach.** `make image` no longer uses the host's compiler. ADR 0006's "No containers" is superseded for the build; no container runs on a Node because of this decision.
- **A change to the Dockerfile changes the digest** and the pull request carries the new `builder.env`. The release runner and a workstation build the same image, and a different digest fails before any build.
- **`make image-repro` compares builds, not a build with itself.** It builds twice in two builder runs with empty build directories, or once against the `SHA256SUMS` of a release another machine built.
- **The reproducibility claim of `image.md` is one claim,** with the assets that differ by design named (the data disk, the test user-data and the inputs file that names the data disk's digest).
- **The snapshot service is a network dependency of building the builder,** not of using it; a machine that holds the image, or loads it from an archive, does not need it.

## Signs the decision was wrong

- The two machines' `SHA256SUMS` differ for an asset that is not on the by-design list, with the same builder digest: the builder does not fix what it was meant to.
- The snapshot service drops a pinned snapshot, so the builder cannot be rebuilt.
- Contributors without a container daemon cannot build, and the image must again build on a bare host.
