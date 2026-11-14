# ADR 0055: orch and the signer are delivered as a system extension, a squashfs image init mounts read-only

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#57 (part of garamsh/naru#118; plan: garamsh/naru#113). It supersedes item 1 of [ADR 0050](./0050-the-image-carries-orch-and-the-signer-and-init-runs-them-as-separate-users-from-spec-orch-spec-signer-and-spec-metrics.md), which put `naru-orchd` and `naru-signer` in the host initramfs's `/bin`; the rest of ADR 0050 (the three sections, the separate users, the service fields, metrics) holds, and 0050 is not edited beyond an appended erratum. It extends [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) (revisions and apply modes) and [ADR 0025](./0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md) (the upgrade) by reference.

## Context

- **The two daemons made the initramfs larger for every Node.** Orch and the signer run on the control hosts only, yet `/bin/naru-orchd` and `/bin/naru-signer` were in the initramfs of every host, and so in the kernel file the A/B slots of the ESP hold (ADR 0020, ADR 0037). Growing them grows the trial slot every release must fit.
- **The target is extensions.** `target.md` names system extensions as one of the eight Node concepts: a part of the system that is added to a Node without changing its boot image.
- **The configuration must stay the truth about a Node.** Which code runs on a Node must be in its configuration and nowhere else, and a Node that cannot run what its configuration names must refuse the boot rather than start without it.

## Decision

1. **A system extension is a squashfs image.** `make image` builds one, `naru-orch`, with `mksquashfs -reproducible` and fixed times, holding `/bin/naru-orchd`, `/bin/naru-signer` and `/manifest.json`. `naru-orchd` and `naru-signer` leave the host initramfs. The host kernel gains `CONFIG_SQUASHFS` with `CONFIG_SQUASHFS_ZSTD=y` as its one compressor, and `CONFIG_BLK_DEV_LOOP=y` for init to attach the image; both are in `required.txt` and `naru.config`. One compressor keeps the kernel smaller and the image build a function of its inputs; zstd is the one that compresses Go binaries best at a read speed that does not slow a start.
2. **The manifest names the image.** `/manifest.json` is `{name, version, services}`: the extension's name, the naru version it was built for, and the names of the services whose binaries it holds. One extension carries both daemons, since a Node that runs orch runs the signer (ADR 0050 decision 2).
3. **The data disk stores images, each file named by its SHA-256** (`/var/lib/naru/extensions/<sha256>.sqfs`, with the manifest read at delivery kept beside it as `<sha256>.json`). More than one image of a name may be stored, for the versions a Node may boot.
4. **The configuration names the extensions.** `spec.extensions` is a list of `{name, sha256}`, keyed by `name` for a merge patch. At boot, before any service and after the registry files, init takes each entry in turn: it picks the stored image whose manifest has that name and the version init itself is, requires its digest to be the entry's, checks the digest again as it reads the file, attaches the file read-only to a loop device and mounts it read-only at `/usr/lib/naru/extensions/<name>`, and compares the manifest it mounted with the stored one. A missing image, an image built for another version, a digest that does not hold, or a manifest that disagrees refuses the boot, naming which. A Node that refuses the boot still serves its Node API, so the image can be delivered and the Node booted again.
5. **Services run from the mount.** Init writes the binaries of `naru-orchd` and `naru-signer` as `/usr/lib/naru/extensions/naru-orch/bin/...`. Validation refuses `spec.orch` or `spec.signer` without a `naru-orch` entry in `spec.extensions`, and refuses a service of `spec.services` that runs from an extension no entry names. `GET /v1/services` names the extension of each service that runs from one (`extension`), and `GET /v1/extensions` lists every stored image with its manifest, size, the services it supplies, and where it is mounted.
6. **Delivery is the Node API.** `PUT /v1/extensions/<sha256>` streams the body into a temporary file in the store, refuses a body whose digest is not the one in the path, mounts the file once to read and check its manifest, and renames it into place. `DELETE /v1/extensions/<sha256>` removes an image that is not mounted. Both need the role `naru:admin`; `GET` needs `naru:reader`. A body larger than the store's bound is refused before it fills the data disk, and an internal error is never put in a response.
7. **`spec.extensions` changes at a boot.** An apply that changes it is refused in `no-reboot` mode and accepted in `staged` and `reboot`: a mounted image is not swapped under a running service.
8. **naructl.** `naructl extension add <file>` hashes the file and streams it with its digest, then prints the `spec.extensions` entry; `extension list` and `extension remove <sha256>` follow the endpoints. `naructl gen node` takes a repeatable `--extension <name>=<sha256>`, and refuses `--signer` and the orch flags without `--extension naru-orch=...`.
9. **The release carries the extension.** `naru-orch-ext-<tag>.sqfs` and `naru-orch-ext-<tag>.json` (name, version, services, `sha256`) are release assets; `make release` checks that the description states the image's digest and the release's version. The release workflow installs `squashfs-tools`. `make image-repro` compares the extension's digest as well as the image's.

## Rejected alternatives

- **Keep the daemons in the initramfs and start them only when configured.** Nothing in the configuration then says which code a Node holds, and the trial slot stays as large as it was.
- **A tar or a directory of files on the data disk.** A directory is not one thing to hash, so the configuration could not pin it by one digest; init would have to unpack it onto a writable file system, and an extension's files would be writable after the boot.
- **EROFS in place of squashfs.** The PM chose squashfs for this decision; both are read-only and compressed, and nothing in the requirements separates them.
- **A digest-free `name` only in the configuration.** The image is delivered over the network by someone other than the author of the configuration; without the digest, init would run whatever was last delivered.
- **Naructl reading the release's JSON for `gen node`.** `--extension <name>=<sha256>` keeps naructl free of the release's file format; the operator reads the `sha256` field of `naru-orch-ext-<tag>.json` once.
- **Mounting the file without a loop device.** A file system image can be mounted only from a block device, so init attaches the file to a loop device itself (`lib/init/linux`), read-only, and detaches it at the unmount.

## Consequences

- The initramfs, and with it the kernel file of each A/B slot, is smaller by the two daemons; the host kernel is larger by squashfs and the loop driver.
- **An upgrade changes two things.** The digest in `spec.extensions` belongs to one naru version, since the manifest's version must be init's. An operator upgrading a control host delivers the new release's image, patches the new digest into `spec.extensions` in `staged` mode, and then posts the bundle. Posting the bundle first boots the trial slot with a digest and an image built for the old version, which the trial boot refuses; the slot then rolls back (ADR 0025), so the mistake is safe and visible. The old image stays stored for a rollback and is removed with `extension remove`.
- A control host whose data disk is lost boots without its images: the Node refuses the boot and serves its Node API, the operator delivers the image, and the Node boots again. The fleet harness stores the image on each data disk it builds instead, since it does not forward every Node's API.
- The mounted extension is read-only and reachable only by the services configured to run from it.

## Signs the decision was wrong

- A second extension with a different cadence than the release: the version in the manifest, which ties an image to init's version, would then be the obstacle.
- An upgrade of a control host proves error-prone enough that the digest should not be in the configuration: a name with a version range, resolved by init, would then be right.
- A loop device on the data disk is found to cost more than it saves: the daemons would return to the initramfs.
