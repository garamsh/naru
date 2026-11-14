# Signer

What `naru-signer` is: the one process that holds the keys of naru's authorities and issues certificates, so that no other process, orch included, can make an identity. It is the first of garamsh/aurora#811's A2 to A5 built; the roles, the revocation list, the admission statement and the encrypted files it uses are `lib/identity`'s ([README](../../lib/identity/README.md)).

## Current decisions

- **`naru-signer` is a binary of the `host/` module,** with its code in `host/internal/signer` and its composition in `host/cmd/naru-signer`. It imports `lib/identity` and no other unit. The image does not carry it yet.
- **Its directory holds every key and is private.** `--dir` (default `/var/lib/naru/signer`) must be mode 0700 and owned by the user the signer runs as, or it refuses to start. Each file is 0600; a key file with a wider mode, or a directory with some of the authorities and not all, is refused at start.
- **What `init` makes.** The host, operator and orch authorities (ECDSA P-256, ten years), the revocation key (Ed25519), the first root certificate for a certificate request the caller supplies, and the empty revocation list at sequence 1. It records the recovery key's public half and writes the revocation key's public half as a PKIX PEM (`--revocation-public-key`), which orch verifies the list with. A directory that holds authorities is not initialised again.
- **Roles and who may do what.**

  | Call | root | operator | reader, automation, orch, host |
  | --- | --- | --- | --- |
  | `POST /v1/operator` (issue operator, reader or automation) | yes | 403 | 403 |
  | `POST /v1/orch` (issue orch) | yes | 403 | 403 |
  | `POST /v1/revoke` | yes | yes, except a serial that is root's: 403 | 403 |
  | `POST /v1/crl` | the revocation key's signature on the list is the authentication: no role, no request envelope | | |
  | `GET /v1/bundle`, `POST /v1/restore`, `POST /v1/replicate` | yes | 403 | 403 |
  | `POST /v1/host` | the statement must be an operator's or root's | | |

- **Where it listens.** HTTP on a unix socket (`--socket`, mode `--socket-mode`, default 0660); the group that may reach it is the deployment's. `GET /v1/crl` (the signed revocation list) and `GET /v1/ca` (the three authority certificates and the revocation key's public half) answer anyone who reaches the socket. `GET /v1/bundle` carries its signed request in the header `X-Naru-Request`, base64. With `--peer-listen`, a TLS 1.3 listener serves `POST /v1/replicate` and nothing else.
- **Authentication is by signature.** A request is the signed envelope of `lib/identity` (`SignRequest`, `VerifyRequest`): action, expiry, body, the caller's certificate. The signer checks the chain against the operator authority for client authentication, the revocation list, the signature, the action and the expiry, then the role. `identity.Authenticate` and `VerifyRequest` are the authenticator seam: authorisation reads only the `Identity` they return.
- **Host certificates.** `POST /v1/host` takes an admission statement and the host's certificate request. The signer runs `VerifyAdmission` itself, requires the SHA-256 of the request's public key to equal the statement's `hostKeySHA256`, and issues CN=hostID, OU host, server and client authentication, one year, with the issue logged (host ID, serial, key hash, the operator's name and serial). A host ID is a name: letters, digits, `.`, `_`, `-`, at most 128.
- **Other certificates** carry one year. An orch certificate also carries the DNS names and IP addresses of the certificate request, which root authorised by signing the request. Operator, reader and automation certificates carry client authentication only; host and orch carry both. Root is made only by `init`.
- **Revocation.** A revoke adds a serial, signs the list again with the next sequence number and writes it before the signer uses it. A serial already on the list changes nothing. The signer records the role of every serial it issues in `issued.json` of its directory (0600, written before the certificate is handed out, carried by the bundle), and a revoke of a serial recorded as root's needs root. A serial the signer has no record of, one issued before the record existed, is not protected.
- **Replicas share one list.** `POST /v1/crl` takes a signed list and answers with the list the signer holds afterwards; the revocation key must have signed it, else 403. A list that adds no serial to the signer's own changes nothing. A list with a higher sequence whose serials are a superset of the signer's own is adopted and written. Lists that each hold a serial the other lacks are merged and signed again at the higher sequence plus one. Orch carries the lists between the signers ([orch.md](./orch.md)); each signer's own sequence still rises by its own revokes, so two signers that merge independently sign different lists at the same sequence, and the next sync merges those again until they agree.
- **Bundle and restore.** `GET /v1/bundle` returns every authority key sealed to the recovery key's X25519 public half. `POST /v1/restore` fills a blank directory from a bundle, the path of the recovery key's file and the path of its passphrase file, and root's authorisation signed for the SHA-256 of that bundle; the authorisation is verified against the bundle's own authorities. The certificates issued before the loss verify against the restored authorities. The recovery key is made outside the signer and never enters its directory.
- **Replication.** A blank signer's `join-key` makes an ephemeral X25519 key; root signs an authorisation for its host ID and that key; `join` posts it to a running signer's peer listener, pinned to the orch authority, and receives the bundle sealed to the ephemeral key. The peer listener's serving certificate is signed by the orch authority in memory with no key on disk. The new signer verifies root's authorisation against what it received before it writes, and removes the ephemeral key.
- **No authority key leaves the directory unencrypted.** The bundle and the replication answer are sealed; the operator's key is in its encrypted person file; the recovery key is encrypted. A test scans a bootstrap's temporary root for private keys outside the directory.
- **`naructl operator add|revoke`** signs a request with the person file given by `--admin` (opened with `--passphrase-file`) and sends it to the socket; `add` takes the certificate request by path and writes the issued certificate.

## Why

- **A separate process with a private directory** is what keeps orch from minting identities: orch asks, and the signer verifies a signature orch cannot make. ([ADR 0040](./adr/0040-naru-signer-alone-holds-the-authority-keys-in-a-private-directory-and-issues-certificates-over-a-unix-socket-against-signed-requests-and-a-recovery-key-opens-the-bundle-that-restores-them.md))
- **The threat model** is orch, and any other user of the machine, reading the keys. That orch's user cannot read the directory is enforced by ownership at deployment (the user `naru-signer`, the directory 0700), and is stated as such; the signer checks the directory's mode and owner and cannot check how orch is run.
- **Accepted risks:** a signed request can be replayed until it expires; an operator can revoke any serial the signer has no record of as root's, and the recovery key reissues root; anyone who reaches the socket may push a list the revocation key signed, which is a replay of an older list at worst and never removes a serial; a host certificate lasts a year with no renewal and no subject alternative name; the bundle and the recovery key are the one recovery path; the join is TLS with server authentication, and the binding of root's authorisation to the ephemeral key is its client authentication.
- **The operator's key is a file behind `crypto.Signer`,** so a hardware key replaces the file without a change to a caller.

## Open questions

- **aurora#811 A4 is not built:** `naructl login` with a second factor and short-lived certificates, re-confirmation of dangerous actions, passkeys, hardware keys and single sign-on.
- **Renewal of a host certificate,** and a subject alternative name for it.
- **Placing the signer in a deployment:** its user, its socket's group and its place in the image are not decided.
- **A command to make an operator's key, certificate request and person file,** to authorise a replica, and to reissue root from the recovery key, does not exist; the library calls do.
