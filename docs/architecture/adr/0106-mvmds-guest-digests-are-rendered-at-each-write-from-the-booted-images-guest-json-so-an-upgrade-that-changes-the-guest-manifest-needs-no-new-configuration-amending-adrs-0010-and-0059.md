# ADR 0106: mvmd's guest digests are rendered at each write from the booted image's guest.json, so an upgrade that changes the guest manifest needs no new configuration

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#363, part of goal garamsh/naru#262. It amends [ADR 0010](./0010-the-guest-kernel-and-initrds-are-image-components-built-at-the-pinned-mvmd-commit.md) by reference (item 3: "mvmd's configuration names these paths and digests, read from `guest.json`" was read by `naructl` once, at `gen node`) and [ADR 0059](./0059-orch-admits-a-host-that-boots-from-an-install-seed-allocates-its-machine-prefix-and-builds-its-configuration-from-one-fleet-profile.md) by reference (the fleet profile carried the guest manifest). It extends [ADR 0025](./0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md): the trial boot is judged on mvmd running steadily.

## Context

- **Every upgrade across a changed guest manifest rolled back on aurora**, on every host (#363): the manifest changed in d93cfa7 (the guest initrd's digest), each Node's stored `/run/mvmd/config.json` still named the old digests, mvmd exited 1 with `firecracker initrd has digest …, not the configured …`, and the trial boot never reached "services running steadily". The upgrade held only after the operator regenerated the file with the new manifest and applied it before the bundle.
- **The digests were stored twice.** The image carries `guest.json` and the files it names (ADR 0010); `naructl gen node` read a copy of that manifest once and wrote its digests into `spec.files`, and orch's fleet profile kept a copy too and wrote them into every joined host's configuration (ADR 0059). The configuration outlives the image, so an upgrade that changes the manifest leaves the copy wrong.
- **A joined host's stored configuration is redacted.** The file is mode `0600`, so `GET /v1/config` shows it as `REDACTED` and the operator cannot read it, patch it or regenerate it from the host: aurora used another Node's configuration with the same prefix as the stand-in.

## Decision

1. **A file of `spec.files` may be a template of the booted image's guest manifest.** The field `template`, whose only value is `guest`, makes init render `content` as a Go `text/template` over the JSON of `/usr/lib/naru/guest/guest.json` (`wire.GuestManifest`), with one function, `json`, at each write of the file: at boot and in an apply. A key the template names and the manifest lacks, a manifest init cannot read and a file that does not render each refuse the write, so the boot or the apply; a template that does not parse is refused at validation as `spec.files[].template`. Init reads nothing else of the file: the template is the tooling's, as the content of every other file is (ADR 0012).
2. **`lib/layout` writes mvmd's configuration as that template.** The kernels map, the default kernel, the guest initrd's and the converter's initrd's paths and digests, and the converter's kernel (the default one) are `{{json …}}` actions; everything else, the Firecracker digest, the network, the registry and the supervision, is written as before. A `{{` in a string the operator gave is written as text.
3. **No guest manifest is an input of the layout.** `Node.Guest`, `HostProfile.Guest` and the `--guest` flag of `gen node` and `gen host-profile` are removed, and with them `layout.Manifest` and its checks: the manifest is the image's, and the image checks its own at build (ADR 0010). The profile orch stores therefore holds no copy, and a joined host's configuration, built from it at admission, holds none either.
4. **An upgrade is derived from the bundle's own image.** The bundle's kernel embeds the initramfs that carries `guest.json` and the guest files; the trial boot renders mvmd's file from that `guest.json`, and a promotion keeps the configuration as it was. There is no step between the bundle and the boot, and a rollback boots the old image, which renders the old digests from its own `guest.json`.
5. **The test image of a changed manifest is built, not faked.** `make image-upgrade` builds a `guest` variant (version `<VERSION>-guest`) on the package `guest-initrds-changed`, the Guest Agent's initrd with 512 zero bytes after its cpio trailer, which the kernel's unpacker skips: a valid initrd of another digest, so the variant's `guest.json` differs from the image's. The fleet and Node tests upgrade to it, and a control keeps the old digests in the stored file and sees the trial roll back.

## Rejected alternatives

- **`naructl` or orch rewrite the file at upgrade.** Orch is not in the path of a host's own upgrade (ADR 0084 rejects the same for extensions), and a rewrite of the stored configuration without an operator's revision is the second copy again.
- **Init knows mvmd's configuration and writes its guest section.** ADR 0003 and config.md keep init ignorant of any service's internals; a template keeps the knowledge in `lib/layout`, which already owns the file.
- **A manifest key per service: `spec.guest`.** A new section for one file; the template field serves any file that needs a value of the image and names no service.
- **Keeping `--guest` as an ignored flag.** An input nothing reads is a second place to be wrong.
- **A patch tool for the joined host (`naructl` reads the profile and writes the file).** The joined host's configuration is redacted, which is the reason the hand step was impossible there.

## Consequences

- An upgrade across a changed guest manifest holds with no step of the operator's, on a host booted from a seed as on one with a configuration of its own.
- **A configuration stored before this change still pins the old digests.** A host whose stored `/run/mvmd/config.json` was written by an earlier `naructl` or by orch from an earlier profile gets the template only from a configuration that carries it: the first upgrade to a release with this decision still rolls back when the manifest changes, unless the operator applies a configuration generated by this release's `naructl` first (`naructl apply --mode staged`). After it, no upgrade needs one. **A stored fleet profile that still has a `guest:` key is refused** by the strict reader (ADR 0059 refuses every key the type lacks, which is how a profile with a secret is refused), so orch refuses to admit a new host, naming the key, until the operator puts a profile written by this release's `naructl gen host-profile`; hosts already admitted are not touched.
- Firecracker's digest is still stored in the configuration and still drifts the same way when a release moves Firecracker; it is not the guest manifest and is not changed here.
- The hand-written stand-in Nodes of `tooling/qemu` (`node.Config.MvmdJSON`) still read the manifest once, at generation; they are test inputs of a Node that is not upgraded across a manifest.

## Signs the decision was wrong

- A second file needs a value of the image: the template would then want more than the guest manifest as its data.
- A template's failure at boot refuses a Node an operator could have kept running with a hand-patched file: the render error would need a way past, which this decision does not give.
