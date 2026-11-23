# ADR 0105: an add-on's image archive names its image by the reference its manifests use and the Node imports it under that name, amending ADR 0083 item 3

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#362, part of goal garamsh/naru#262. It amends item 3 of [ADR 0083](./0083-a-control-plane-node-applies-pinned-cluster-add-ons-from-its-image-with-an-init-service-and-every-node-imports-their-images-so-coredns-and-a-local-volume-provisioner-run-by-default.md) by reference. The facts about containerd are those of v2.4.2, the version `image/pkgs/containerd/pin.env` pins, read from `ctr` and `containerd` of that release run against a scratch root.

## Context

- **The kubelet could not find an add-on's image.** On commit d93cfa7 both CoreDNS Pods stayed in `ImagePullBackOff` and the kubelet asked the registry `naru.local` for `naru.local/coredns@sha256:67cc62f6717bba5b420b9997c382f1b1fb4eeb938d2d57b1230d34bac7d73744`, while the import of that Node had logged `naru.local/coredns@sha256:3cee6c781e5099687a812f3de900217146e716abe6847138db075908f276cee7` and `naru.local/coredns:release` (`3a-dns.log` and `3b-volumes.log` of that run). DNS, volumes and NetworkPolicy tests that need a Pod failed with it.
- **The manifests named the digest of the image's manifest; the import named the digest of the archive's index.** Each package wrote `naru.local/<name>@sha256:<digest of manifest.json>` into its manifest and named the archive's one image `naru.local/<name>:release`. `ctr images import --digests --base-name naru.local/<name>` adds a second name, `naru.local/<name>@sha256:<digest of index.json>`, whose target is the index. Reproduced with the pinned `ctr` against a scratch containerd: the manifest digest was `a6b7b344…`, the index digest `435b0f8b…`, and the store held `naru.local/coredns:release` and `naru.local/coredns@sha256:435b0f8b…`, and no name with `a6b7b344…`.
- **The same defect was in all three add-on packages**, `coredns`, `volumes` and `ccm`, which each laid out the archive and wrote the reference with their own copy of the commands.
- **A name the import does not create fails silently.** Run with `--base-name naru.local/coredns`, `ctr` imported an archive whose image name was outside that prefix with exit status 0 and created no image.

## Decision

1. **The archive names its image by the reference its manifests use**: `naru.local/<name>@sha256:<digest of the image's manifest>`, in the `io.containerd.image.name` annotation of its index. That string is made once, by `image/pkgs/addon-image.sh build`, which prints it; the package writes the printed string into the manifest, and nothing else spells it.
2. **The Node imports each archive under the name it carries.** The `addon-image-<name>` service runs `ctr --namespace k8s.io images import --label io.cri-containerd.pinned=pinned <archive>`, with no `--base-name` and no `--digests`, so the Node names nothing and `ctr` takes the archive's name as it is. The image's target is the image's manifest, and the digest of the name is the digest of the target.
3. **The build checks that the two agree.** `addon-image.sh check` runs in each package after the manifest is written and fails the build unless the archive names exactly one image as `naru.local/<name>@sha256:<d>`, `d` is the SHA-256 of the blob the index points at, and every `naru.local/` reference in the package's manifest is that name.
4. **The tag `naru.local/<name>:release` is no longer created.** No manifest, document or test referenced it.

## Options considered

- **Keep `--digests` and write the index digest into the manifests.** The manifest would then depend on how `ctr` derives a name from the archive, which the pinned release decides (the importer of v2.4.2 handles `SkipNamedDigest` differently from that of v2.1.4), so the two would agree by an accident of the tool.
- **Check on the Node after the import.** The build already knows both strings, and a check there fails the release, not a Node at boot.

## Consequences

- The names of the three images change with the archives' bytes: each digest differs from the one the previous build had, because the index differs.
- An archive named outside `naru.local/<name>@sha256:<digest>` is refused when the package is built; a Node that imported a wrongly named archive would still exit 0 without creating the image, which only the build check and the fleet test catch.
- The SBOM's digest of each add-on image (`ReadAddonImages`) is unchanged: it is the digest of the manifest the index names.

## Signs the decision was wrong

- A containerd release whose import rewrites the name an archive carries: the fleet test `TestNode_EveryAddonImageIsFoundByTheKubeletUnderTheReferenceItsManifestNames` fails first.
