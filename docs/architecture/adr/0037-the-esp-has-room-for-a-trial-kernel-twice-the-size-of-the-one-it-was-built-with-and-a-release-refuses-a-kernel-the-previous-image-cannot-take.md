# ADR 0037: the ESP has room for a trial kernel twice the size of the one it was built with, and a release refuses a kernel the previous image cannot take

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#106. It amends [ADR 0020](./0020-a-node-boots-one-kernel-file-per-slot-of-the-esp-with-the-initramfs-embedded-and-a-trial-slot-is-reached-once-by-bootnext.md) and [ADR 0025](./0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md) by reference: their bodies are not edited, and what they say holds with this decision added, except the ESP's size, which item 1 replaces.

## Context

- **The ESP was sized for two kernels of the built image,** the larger of 64 MiB and two kernels with 8 MiB, a whole number of MiB. The kernel of v0.1.0-dev.7 is 79270912 bytes, which gave a 160 MiB ESP.
- **The kernel of v0.1.0-dev.8 is 211239936 bytes,** 2.7 times that. Its bundle does not fit the trial slot of a Node installed from the dev.7 image: the default slot holds 79270912 bytes, and the room left is under 80 MiB.
- **Nothing refused the release.** `make release` built the bundle, and the failure was found when a Node was upgraded.
- **FAT32 at 512-byte clusters costs more than the files.** Measured with `mkfs.fat -s 1`: 1.0 MiB at 64 MiB, 2.48 MiB at 160 MiB, 15.77 MiB at 1 GiB, 31.5 MiB at 2 GiB, which is about the partition's size over 64 and a MiB.

## Decision

1. **The ESP is the larger of 1 GiB and `2 x 2 x kernel + 8 MiB`,** a whole number of MiB, where kernel is the size of the kernel the disk is built with. Each of the two slots can then hold a kernel twice the built one's. The boot disk grows by the difference.
2. **The bundle's `manifest.json` records the ESP size** as `esp.bytes`: the size of the ESP of the image built with the release's kernel.
3. **`make release` refuses a bundle that would not fit the trial slot of the previous release's image.** `PREV_MANIFEST=<local path>` names that release's `manifest.json`. A kernel fits when its size is at most the ESP, less the ESP over 64, less a MiB, less the previous kernel. The ESP is the manifest's field, or, where a release has none, `max(64 MiB, 2 x kernel + 8 MiB)` rounded up to a MiB, which is what the builder of that time gave it. The refusal happens before the long build steps.
4. **A release marked as requiring recreation is let through.** `RECREATE=1` (the workflow's `requires_recreation` input) lets the release be built; the release's notes then state that Nodes installed from an earlier image are recreated. With no `PREV_MANIFEST`, nothing is checked and the command prints that.
5. **The release workflow fetches the previous release's bundle,** takes `manifest.json` out of it, and passes the path. With no earlier release it passes none.

## Consequences

- A Node installed from an image with this decision takes an upgrade to a kernel twice the size of its own. A Node installed before it does not gain room: an upgrade it cannot take is refused at release time, and an operator recreates it.
- The boot disk is at least 1 GiB larger than before; the qcow2 stays sparse. Building and writing a disk takes longer by the copy of the kernel into a larger FAT.
- The 8 MiB of slack is smaller than the FAT's own cost above kernels of about 250 MiB. The 1 GiB minimum covers kernels up to 254 MiB; beyond that the next release's refusal (item 3) is the guard, not the formula.
- The release check reads a kernel size and an ESP size from a manifest, not the disk of the previous image, so it is a model of FAT32's cost with a margin, not a measurement.

## Signs the decision was wrong

- A Node of an image built with this decision cannot take an upgrade the release check let through: the model of FAT32's cost is too thin, and the check measures it.
- Kernels approach 254 MiB: the 1 GiB minimum no longer carries them, and the slot sizes need a decision of their own.
