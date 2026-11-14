# ADR 0046: the host API adds and removes a boot set on a running host through mvmd's runtime API, and a release carries the Node boot set as a kernel, an initramfs and a manifest

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#98 part C (phase N1 of #113). It extends [ADR 0028](./0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md), [ADR 0034](./0034-a-node-boots-as-a-microvm-from-a-guest-kernel-and-initramfs-the-image-carries-as-a-boot-set.md) and [ADR 0035](./0035-the-host-api-creates-a-microvm-of-an-os-form-from-a-boot-set-with-a-data-disk-and-a-nocloud-seed-and-the-iso-9660-writer-is-a-library-module.md) by reference: their bodies are not edited, and what they say holds with this decision added.

## Context

- **A Node microVM boots a boot set the host knows by name.** Until now the only one was the `naru-node` boot set the host image carries (ADR 0034), so a new Node version needed a new host image.
- **Decision 4 of #113 makes `naru-node` an artifact of its own,** a kernel and an initramfs a host runs at its own version and the one before, so a host must take a boot set after it has booted, with no reboot. mvmd provides this through its runtime API: `AddBootSet`, a client stream that carries the name, the SHA-256 of each file and the bytes of the kernel and then the initramfs, and `RemoveBootSet`, which refuses a set a Machine names (garamsh/mvmd#746).
- **mvmd's names for a boot set are `[a-z0-9-]`, 1 to 63 characters.** A release tag such as `v0.4.0` has dots.
- **A kernel and an initramfs are tens of megabytes.** The host API's other bodies are JSON of a few kilobytes.
- **The operator reaches the host through naructl.** The files are on the operator's disk, beside the release's other assets.

## Decision

1. **`POST /v1/boot-sets`** (the `naru:host-provisioner` role only; a reader is 403) takes `multipart/form-data`, read as a stream and never held whole in memory, with three parts in this order: `boot-set`, a JSON `{name, kernelSha256, initramfsSha256}` of at most 4 KiB; `kernel`; `initramfs`. Each file is at most 1 GiB and the request is bounded to 15 minutes. hostd passes the digests to mvmd, which checks the SHA-256 of each file as it receives it and refuses a file that differs. hostd does not hash the files itself.
   - 200 with the request echoed when mvmd has the set. The same name with the same digests again is 200.
   - 409 for a name mvmd holds with other digests.
   - 422 for a name outside `[a-z0-9-]{1,63}`, a digest that is not 64 hexadecimal characters, or a file whose SHA-256 is not the one named.
   - 400 for a body that is not multipart, whose parts are not these three in this order, or that is cut short; 413 for a file over the limit; 503 when mvmd does not answer.
2. **`DELETE /v1/boot-sets/{name}`** (provisioner only) answers 204, also for a set mvmd does not hold, so a repeat is safe. It is 409 while a Machine names the set, and 422 for a name outside the pattern.
3. **hostd's mvmd interface gains `AddBootSet` and `RemoveBootSet`** over the runtime API at the pin the host module requires (garamsh/mvmd `b1a60cd64967`, which carries `AddBootSet`). Authorization is unchanged: a non-GET request needs the provisioner role.
4. **A release carries the Node boot set as three assets:** `naru-node-kernel-<tag>`, `naru-node-initramfs-<tag>` and `naru-node-<tag>.json`, the manifest `{name, version, kernel: {file, sha256, bytes}, initramfs: {file, sha256, bytes}}`, whose `file` is the name of the asset beside it. The kernel is the guest form's `naru-node` vmlinux and the initramfs is `node-initrd.cpio.gz`, both of `make image`. `SHA256SUMS` lists them. A release is 12 files: eleven assets and `SHA256SUMS`.
5. **The boot set's name is `naru-node-` and the tag in lower case with each character outside `[a-z0-9-]` replaced by `-`,** so `v0.4.0` is `naru-node-v0-4-0`. The release refuses a tag whose name is over 63 characters. The manifest's `version` keeps the tag as it is. This differs from the name `naru-node-<tag>` of the brief of #98 only where the tag has a character mvmd's names do not allow.
6. **naructl** gains `host boot-set add <naru-node-<tag>.json>`, which reads the manifest, opens the two files it names from the manifest's directory (a `file` that is not a plain file name is refused), and streams them to `POST /v1/boot-sets` as above with the digests from the manifest, with a 15-minute limit; and `host boot-set remove <name>`. naructl and the release each define the manifest's struct, because units do not import each other; this ADR and [image.md](../image.md) are the contract.
7. **The release's notes state two figures:** the ESP size of the boot disk and the largest kernel a Node installed from it can take as an in-place upgrade (`TrialRoom` of its own manifest, ADR 0037). `naru-release` writes them to the file `-notes-figures` names, outside the assets directory because every file there is published; the build job reads them into job outputs and the publish job puts them in the notes.
8. **The release workflow's count of files is 12,** and the notes list the three assets.

## Consequences

- A host takes a new Node version with `naructl host boot-set add` and no reboot, and a Node microVM is created from it by name.
- A boot set a microVM names cannot be removed: the caller deletes the microVM first.
- A release's tag decides a name, so two tags that differ only in characters mvmd's names do not allow (`v1.0` and `v1-0`) name the same boot set. The same name with other digests is 409, so the second is refused and not silently merged.
- hostd holds no file in memory and no file on disk; its bound is the time limit and the size limit.
- The QEMU test of this change is written and was not run when the pull request was opened.

## Signs the decision was wrong

- A real tag makes a name collision between two releases: the name takes the tag's digest or a counter.
- A kernel or an initramfs grows past 1 GiB, or an upload takes more than 15 minutes on the networks hosts are on: the limits move or the transfer is made resumable.
- A caller needs to add a boot set from a place that is not the operator's disk (a registry, a URL): the API takes a reference as well as bytes.
- Hashing in mvmd alone leaves a bad file in the host's boot set directory for a moment: hostd checks the digests before it calls mvmd.
