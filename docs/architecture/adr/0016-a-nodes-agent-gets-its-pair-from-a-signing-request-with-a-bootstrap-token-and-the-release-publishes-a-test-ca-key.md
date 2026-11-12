# ADR 0016: a Node's agent gets its pair from a signing request with a bootstrap token, and the release publishes a test CA key

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#37, part of goal garamsh/naru#34. It adds to [ADR 0013](./0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md) and [ADR 0015](./0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md), and it supersedes one consequence of ADR 0015, item 3: "node-1's file carries as certificates and no CA key" (Decision 5 below). The rest of both stands; [ADR 0012](./0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md)'s body is not touched.

## Context

- **gaon can issue a Node Agent's pair itself.** At gaon commit `c2b42f91724cae39a4f6824517c278150cf4695f` (gaon ADR 0007) an agent that holds a bootstrap token may create and read its own CertificateSigningRequest; `gaon-controllers` is the signer and approves a request whose CN is the Node's name; the agent keeps the pair it is issued in `--cert-dir`. The API server takes the tokens from `--bootstrap-token-file`, the controllers sign with `--signing-cert` and `--signing-key`.
- **Until now naru's tooling issued every agent pair**, as a file (ADR 0012) or from `spec.pki` (ADR 0013), so the agent's key was in the configuration or was made by init for a service that never asked for one.
- **A signer needs the CA's key where the controllers run.** With `spec.pki` that is the control-plane Node, which already holds the key for init (ADR 0013).
- **The release's test user-data had no CA key** (ADR 0015 item 3), so it could not use `spec.pki`, and the release's Nodes could not exercise the path an operator uses.

## Decision

1. **A Node's agent gets its pair by a signing request, with a bootstrap token.** The agent runs with `--tls-ca /run/gaon/ca.pem --bootstrap-token-file /run/gaon/bootstrap-token --cert-dir /var/lib/naru/gaon-node-agent` (a directory on the data disk, so the pair survives a reboot and the agent reuses it). Init issues no agent pair.
2. **A token is `[a-z0-9]{6}\.[a-z0-9]{16}` from `crypto/rand`**, one per Node, written by `naructl` to `<ca-dir>/tokens/<name>` (mode `0600`) and into the Node's `/run/gaon/bootstrap-token` (`spec.files`, `0600`). The control-plane Node also carries `/run/gaon/bootstrap-tokens`, one line per Node, which the API server names with `--bootstrap-token-file`; the controllers get `--signing-cert /run/gaon/ca.pem --signing-key /run/gaon/ca.key`, a path `spec.pki` writes the authority's key to (`keyPath`, mode `0600`, ADR 0013).
3. **`naructl gen token --ca <dir> --name <name>`** makes a Node's token if it has none. `gen node` makes its own if it is missing, and a control-plane `gen node` lists every token present at that time. An operator adds a Node by `gen token` for it, `gen node` for it, then `gen node --control-plane` again for the control-plane Node and a new user-data for it ([config.md](../config.md#operator-tooling-naructl)).
4. **`--pairs-as-files` stays, as the operator's-pair form for any Node.** The Node carries a pair issued by `naructl` from the CA as files; a control-plane Node with it has no `spec.pki`, no CA key, no token and no signing flag. `naructl` refuses a combination that cannot work: a Node with no token, a malformed token, a list on a worker or one that lacks the control plane's own token, `--disable gaon-controllers` on a signer, a token with `--pairs-as-files`.
5. **The release's test user-data takes the token form.** `naru-node-1.test.yaml` carries the build's throwaway test CA, key included, in `spec.pki`, and both files carry a token. The release notes mark it as they mark the registry's test authority ("do not trust the CA anywhere else"). **An operator's CA is never written into any asset**: `naructl gen node` writes it only into the control-plane Node's user-data and the operator's `ca.key`.
6. **Pre-release tags.** The release workflow publishes a tag whose name contains `-` with `--prerelease --latest=false`, so a candidate with a test CA key is never the latest release.

## Consequences

- **The release publishes a CA private key**, in `naru-node-1.test.yaml`. Anyone who reads it can issue a certificate the deployment's API server accepts, and sign a Node's request there. The CA exists for the build, is valid ten years, and trusts nothing but that deployment. This supersedes the "no CA key" of ADR 0015 item 3; [image.md](../image.md#release-assets) names the keys.
- **A bootstrap token is a credential that is in the configuration.** Whoever reads a Node's user-data holds that Node's token until it is removed from the control plane's list; `naructl` does not renew one. A worker's agent key is made on the worker and never leaves it.
- **Adding a Node needs the control-plane Node's user-data again**, because the API server reads its token list at start.
- **The agent's certificate directory is on the data disk**, so an agent that reboots does not request again; a data disk that is replaced makes the agent request again with the token it still carries.
- **The judge's list of issued pairs shrinks** to the API server's, the scheduler's and the controllers' (`GaonIssued`).

## Signs the decision was wrong

- An operator cannot remove one Node's token without rewriting the control plane's user-data, which would say the token list belongs in gaon's API and not in a file.
- A release's test CA key is used against a deployment that held something of value, which would say the release should generate the CA at deploy time instead.
- The agent requests a pair on every boot, which would say `--cert-dir` is not where gaon keeps what it is issued.
