# Verify a release

What a consumer checks before using the assets of a release of `garamsh/naru`, and what each check proves. [ADR 0076](../architecture/adr/0076-a-tag-publishes-once-and-a-release-is-signed-attested-and-carries-an-sbom-and-the-corresponding-source.md) decides what a release carries; [image.md](../architecture/image.md) says how it is built.

Values in angle brackets are yours: `<tag>` is the release tag (`v0.1.0`), `<asset>` a file of the release. Download the release into one directory first: `gh release download <tag> -R garamsh/naru`. The commands need `cosign` (v3), `gh` and `sha256sum`.

## 1. The signature on SHA256SUMS

```
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity 'https://github.com/garamsh/naru/.github/workflows/release.yml@refs/tags/<tag>' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  SHA256SUMS
```

It prints `Verified OK` when the release workflow at that tag signed this `SHA256SUMS`. A release made by running the workflow by hand (`workflow_dispatch`) is signed as the workflow at the ref it was dispatched from, so use `--certificate-identity-regexp 'https://github.com/garamsh/naru/.github/workflows/release.yml@.*'` for it.

## 2. The assets against SHA256SUMS

```
sha256sum --check --ignore-missing SHA256SUMS
```

Every asset present is printed `OK`. `--ignore-missing` lets you download only the assets you use.

## 3. The provenance of an asset

```
gh attestation verify <asset> -R garamsh/naru
```

It proves the asset was built by the release workflow of the repository, from the commit the attestation names.

## The SBOM and the sources

`naru-sbom-<tag>.spdx.json` lists every package of the image and every Go module of the programs the release ships, with licences. `naru-sources-<tag>.tar` holds the upstream tarball of every GPL-, LGPL- and AGPL-licensed package (`sources/`) and `naru-source.tar`, the source of this repository at the built commit. Both are listed in `SHA256SUMS`, so steps 1 and 2 cover them.

## One tag, one release

A tag publishes once. Running the workflow again for a tag whose release exists compares `SHA256SUMS` with the published one, uploads nothing when they agree, and fails when they do not (the data disk and the test user-data differ in every build by design and are left out of that comparison).
