# ADR 0078: The release's test user-data derives its authority and tokens from the version, so only the data disk differs between builds

> Status: accepted
> Date: 2026-11-16

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#239, part of goal garamsh/naru#235. It changes [image.md](../image.md)'s Release assets and Reproducibility, and it amends one consequence of [ADR 0016](./0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md) decision 5 ("the build's throwaway test CA ... generated for each build") and the exemption of the test user-data in [ADR 0076](./0076-a-tag-publishes-once-and-a-release-is-signed-attested-and-carries-an-sbom-and-the-corresponding-source.md) decision 1. The rest of both stands.

## Context

- **Two builds of one commit differed in `naru-node-1.test.yaml` and `naru-node-2.test.yaml`.** Each `make release` ran `naructl gen ca` and `gen token`, so every build had a new CA, new tokens and new files. `image/repro.sh` and `ByDesignPattern` left the two files out, which is two of the three assets a consumer could not compare.
- **The key is published already.** ADR 0016 publishes the CA's private key in `naru-node-1.test.yaml` and the tokens in both files, for a deployment nothing depends on. Nothing in them is secret, so deriving them from a public value gives away nothing the asset does not.

## Decision

1. **The test CA and the tokens derive from the release's version.** `internal/release` writes the CA directory that `naructl gen node` reads (`ca.pem`, `ca.key`, `tokens/<name>`) itself: the P-256 key and the certificate's serial are HMAC-SHA-256 outputs keyed by the version, the certificate is valid from `SOURCE_DATE_EPOCH` less an hour for ten years, and each token is the `[a-z0-9]{6}\.[a-z0-9]{16}` form drawn from the same HMAC. The certificate's signature is deterministic (RFC 6979), because `crypto/ecdsa` mixes fresh randomness into every signature whatever reader it is given. The files `naructl` wrote before are the same in kind: only their values are no longer random.
2. **Two builds of one commit and one `SOURCE_DATE_EPOCH` write the same user-data.** Two versions write different CAs and tokens, and nothing else differs between them.
3. **Only the data disk is left out of a comparison.** `image/repro.sh`, `ByDesignPattern` (and so `naru-release compare`) exempt `naru-data-*.qcow2` and nothing else.
4. **The files stay assets.** Their consumers (a Proxmox deployment, `make nodes` and the judge) keep working; dropping them would have removed the release's example of the token form.

## Consequences

- **Anyone can compute a release's test CA key from its version.** It was readable in the asset, so the exposure is unchanged. A deployment that held something of value was already outside ADR 0016.
- **A CA is trusted only by a deployment that took its user-data from a release of that version.** A deployment of one release does not trust another's, because the versions differ.
- **The CA is valid for ten years from `SOURCE_DATE_EPOCH`** and not from the build. A release built long after its epoch has a shorter life; a new epoch in `image/build.env` moves it.
- **`naructl gen ca` and `gen token` are no longer how the release makes its CA.** The layout they write is one `naructl gen node` reads, and a test builds user-data from a derived directory and has `naru-init -validate` accept it, so a change to the layout fails there.

## Signs the decision was wrong

- A release's test CA is used for something that held value, which would say the derivation hands an attacker a key the asset's reader did not have to read.
- `naructl` changes the layout of a CA directory and the release's derived one is not noticed to be stale.

## Note, 2026-11-17: the registry's authority is derived too

Found by `make image-repro` on garamsh/naru#235: two builds of one commit differed in the two user-data files again, in the certificate of `naru test registry CA`, which `spec.registries` names through `ca=`. `fixture.Stage` makes the registry directory's authority and leaf (`ServerCerts`) from fresh randomness and the build's clock, and decision 2 held only for the test CA. The release now writes `ca.pem`, `cert.pem` and `key.pem` of the staged `inputs/registry` itself (`internal/release/registrypki.go`), over what `Stage` wrote and before the data disk is built, so that the disk's registry and the user-data's `ca=` stay one authority: the keys and serials are HMAC-SHA-256 outputs keyed by the version, both certificates are valid from `SOURCE_DATE_EPOCH` less an hour for ten years, and the signatures are RFC 6979's, as in decision 1. The decision is unchanged; the list of what derives from the version was incomplete. The data disk is still left out of a comparison (decision 3).
