# ADR 0084: an upgrade bundle carries the extensions the host's configuration names, and an entry without a digest is resolved by init to the image built for its version

> Status: accepted
> Date: 2026-11-21

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#265, part of goal garamsh/naru#262. It supersedes [ADR 0055](./0055-orch-and-the-signer-are-delivered-as-a-system-extension-a-squashfs-image-init-mounts-read-only.md) in part: item 4's "an entry is `{name, sha256}`" (the digest becomes optional), and the Consequences paragraph "An upgrade carries both versions for a while" (the four hand steps), which 0055's first sign of being wrong foresaw. It extends [ADR 0025](./0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md) (the bundle) and is the delivery [ADR 0083](./0083-a-control-plane-node-applies-pinned-cluster-add-ons-from-its-image-with-an-init-service-and-every-node-imports-their-images-so-coredns-and-a-local-volume-provisioner-run-by-default.md) names as the prerequisite of shipping the volume driver.

## Context

- **aurora's node-3 rolled back from dev.11 to dev.13 with `extension "naru-orch": no entry is built for naru v0.1.0-dev.13`** (#263): the operator missed delivering the new image and adding its entry. ADR 0055 requires four hand steps per control host per release, and garamsh/naru#263's 422 only names them.
- **An image belongs to one naru version** (the manifest's version must be init's), and the configuration pins an image by digest. So every release changes the configuration of every control host, which is the operator's truth and not init's to edit.
- **The payloads are growing.** ADR 0083's cluster add-ons (kubectl, CoreDNS, the CSI driver and its sidecars) pass 256 MiB. A squashfs extension is mounted from the data disk and costs no RAM beyond the page cache, unlike the initramfs, so these payloads are extensions of the same kind as `naru-orch`: the mechanism carries more than one.
- **The ESP holds the kernel only** (ADR 0037); extensions go to the data disk.

## Decision

1. **The bundle carries extensions.** `naru-host-upgrade-<version>.tar` keeps `kernel.efi` and `manifest.json` as its first two members; zero or more members `extensions/<name>.sqfs` follow. The manifest gains `extensions`, a list of `{name, bytes, sha256}` of the members that follow, in order. A bundle with no `extensions` is valid, as every earlier bundle is. The whole-body SHA-256 the operator asked for covers the extensions (ADR 0025 item 1), and each member is also checked against the manifest.
2. **Init stages the extensions the configuration names, before it arms the trial.** For each member whose name the stored `spec.extensions` lists, init streams it into the extension store as a temporary file, checks size and digest, mounts it once to check that its manifest has that name and the bundle's version, and renames it into place by SHA-256 (ADR 0055 item 6). A member no entry names is read and discarded: a host that does not run orch does not store it. Every refusal happens before `TRIAL.EFI` is renamed, `Boot0100` or `BootNext` is written, and removes the temporary files: a bad member is `400`, a store with no room is `413`. The bundle's cap is raised to fit the extensions of the release.
3. **An entry may omit its digest.** `{name}` means "the stored image of this name built for init's version". At boot init mounts the one such image; none, or more than one, refuses the boot as 0055 item 4 does, naming which. An entry with a `sha256` keeps 0055's meaning exactly and is how a customer pins an extension of its own. A name has at most one unpinned entry; the key of a merge patch is the digest of a pinned entry and the name of an unpinned one. The version equality that tied an image to init is not loosened: a range adds nothing, since exactly one init runs.
4. **The preflight of #263 follows.** A bundle is refused with `422` when an unpinned entry's name has neither a member of the bundle nor a stored image built for the bundle's version, or a pinned entry has no stored image built for it as before. The message no longer names hand steps for an unpinned entry.
5. **A rollback leaves nothing behind.** Init removes stored images that no pinned entry names and that are built for another version than its own, at the end of a promotion and at a default boot with no upgrade pending (so never between the post and the trial). A healthy trial therefore drops the old image; a trial that rolls back is followed by a default boot of the old version, which drops the new one, and the old image was never touched, so the old init mounts it. A retry posts the bundle again. Pinned images are never removed by init.
6. **Operators do one step.** `naructl upgrade` is the whole procedure; a control host's entry is `{name: naru-orch}` written once by `naructl gen node --extension naru-orch` (the form without `=<sha256>`). An add-on of ADR 0083 is an extension in the same bundle, named by an entry of the cluster's configuration (ADR 0083 item 12 is the source: the cluster's configuration switches each add-on, `addons: {dns: false}`, and `naructl gen kubernetes` writes the extension entry of each enabled add-on and none for a disabled one, so an add-on is on when its entry is present because the switch put it there, and there is one truth). A payload extension may have an empty `services` list in its manifest (ADR 0055 item 2 required one), since it supplies files.
7. **The release builds the bundle with the extensions.** `image/` assembles the bundle from the release's `naru-orch` extension (and, as they land, the add-on extensions); the standalone `naru-orch-ext-<tag>.sqfs` assets of 0055 item 9 stay, for hosts that pin.

## Rejected alternatives

- **The bundle carries the images and the operator still edits the entry.** Half of the problem stays: the entry is the step aurora missed.
- **Init rewrites the configuration to add the new entry.** The configuration would change without an operator's revision, and a rollback would leave a revision naming an image the old init never saw.
- **Orch delivers the images.** A control host is the one that runs orch; it cannot deliver to itself while it is being upgraded, and a fleet would need orch up, reachable and ahead in version on every host. It also gives orch a second job.
- **A name with a version range.** Init is one version; equality is the whole range.
- **A separate extensions bundle posted before the upgrade.** Two steps, and a mistaken order is exactly the error to remove.
- **Keeping both versions' images on the data disk for ever.** A control host would collect an image per release.

## Consequences

- A control host's upgrade is one command and a rollback lands on the old extension with the new image removed after it.
- An unpinned entry runs whatever image of the right name and version is stored, which an admin can put with `PUT /v1/extensions` as before: 0055's reason for the digest (someone other than the author delivers the image) now holds for the pinned form only, and the unpinned form relies on the bundle's digest the operator chose and on the admin role.
- A bundle is larger by the extensions the host does not use only on the wire: the host discards them.
- A data disk that cannot hold the old and new image at once refuses the upgrade with `413`.

## Signs the decision was wrong

- Hosts need two images of one name and version (a payload variant): the single-candidate rule would then be the obstacle.
- The bundle's size, for the extensions of a release, makes the transfer a problem: a separate store the host fetches from would be right.
- Pruning at a default boot removes an image an operator staged by hand for the next release: staging by hand would then need a pin.
