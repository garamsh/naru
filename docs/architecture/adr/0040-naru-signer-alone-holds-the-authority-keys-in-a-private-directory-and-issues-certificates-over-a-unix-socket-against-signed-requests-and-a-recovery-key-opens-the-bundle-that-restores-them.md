# ADR 0040: naru-signer alone holds the authority keys in a private directory and issues certificates over a unix socket against signed requests, and the recovery key opens the bundle that restores them

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#117 (garamsh/aurora#811 A2 to A5, garamsh/naru#113 decision 8). It builds on `lib/identity` (garamsh/naru#123): the roles, the revocation list, the admission statement and the encrypted files.

## Context

- **No process holds the keys of naru's identities yet.** The host API's authority is a key a person keeps in a secret store (ADR 0028); aurora#811 asks for authorities that issue host, operator, reader, automation and orch certificates, and for orch not to be able to mint any of them.
- **Orch is the process an attacker is most likely to reach,** because it takes requests from callers. If it held the authority keys, reaching it would be reaching every identity.
- **A lost authority key is a lost cluster.** Host certificates already issued chain to it, and a new authority would orphan them.
- **A decision that is not recorded is argued again:** what the signer defends against, what it accepts, and what A4 asks for that is not built.

## Decision

1. **`naru-signer` is a binary of the `host/` module, with its code in `host/internal/signer`.** It holds the authority keys and nothing else holds them. Its subcommands are `init`, `serve`, `join-key` and `join`.
2. **Its directory (`--dir`, default `/var/lib/naru/signer`) must be mode 0700 and owned by the user running it, or the signer refuses to start.** Every file in it is written 0600, and a key file wider than that is refused at start. A directory that holds some authorities and not all is refused.
3. **`init` makes three CAs, the host, the operator and the orch authority,** as ECDSA P-256 X.509 certificates valid ten years, and the revocation key, an Ed25519 key. It issues the first root certificate for a request the caller made, under the operator authority. People and automation share the operator authority; the role is the certificate's one OU.
4. **The API is HTTP over a unix socket (`--socket`, mode `--socket-mode`, default 0660).** Who may reach the socket is the deployment's: the group of the socket. `POST /v1/host` takes an admission statement and the host's certificate request; `POST /v1/orch`, `POST /v1/operator` and `POST /v1/revoke` take a signed request; `GET /v1/crl` and `GET /v1/ca` hold nothing secret; `GET /v1/bundle` takes a signed request in the header `X-Naru-Request`, base64, because a GET has no body; `POST /v1/restore` takes the bundle, root's authorisation and two file paths.
5. **A caller is authenticated by what it signs, not by the connection.** A request is `lib/identity`'s signed envelope: the action, an expiry (naructl uses two minutes) and the body, signed with the caller's ECDSA key, with the caller's certificate attached. The signer verifies the chain against the operator authority, the revocation list, the signature, the action and the expiry, and then checks the role. Root issues operator, reader, automation and orch certificates, takes the bundle and authorises a restore and a replica; root and an operator revoke. An operator, reader, automation or orch identity is refused anywhere else with 403.
6. **A host certificate is issued only for an admission statement the signer verifies itself.** The signer runs `VerifyAdmission` against the operator authority and the revocation list, checks the certificate request's signature, and requires the SHA-256 of the request's public key to equal the statement's `hostKeySHA256`. The certificate is CN=hostID, OU host, server and client authentication, valid one year, and the issue is logged with the host ID, the serial, the key's hash and the operator's name and serial. An orch identity alone cannot obtain one, because orch's word is not taken and the statement must be an operator's or root's.
7. **Operator, reader and automation certificates carry client authentication only,** which `VerifyAdmission` requires of the signer of a statement. Host and orch certificates carry both. All are valid one year.
8. **No authority key leaves the directory unencrypted.** The one way out is the bundle: every authority key, sealed (`lib/identity` `Seal`) to the X25519 public half of the recovery key, which `init` records as the file `recovery-public.json`. The recovery key itself is made by the operator outside the signer, encrypted with a passphrase, and never enters the directory.
9. **`POST /v1/restore` fills a blank directory from a bundle.** The recovery key's file and its passphrase file are paths on the signer's host, in the request. The signer opens the bundle, checks that root's authorisation, signed for the SHA-256 of that bundle, verifies against the bundle's own operator authority, and only then writes. The certificates issued before the loss verify against the restored authorities, and new ones issue.
10. **A second signer is filled by replication.** The new signer makes an ephemeral X25519 key (`join-key`) and root signs an authorisation for its host ID and that key. The signer that holds the authorities listens for peers on a TLS 1.3 listener (`--peer-listen`) whose serving certificate the orch authority signs in memory, with no key on disk, and answers `POST /v1/replicate` with the bundle sealed to the ephemeral key. The new signer (`join`) pins the orch authority, opens the answer, verifies root's authorisation against the authorities it received, and saves them. A unit test runs two signers in one process.
11. **The authenticator seam is `identity.Authenticate` and `identity.VerifyRequest`.** Authorisation reads only the `Identity` they return (subject, role, serial), never how it was proved. A second factor, a short-lived certificate or a hardware key changes what those functions accept and nothing in the signer's handlers. No separate interface is added for it.
12. **The operator's key is a file, behind `crypto.Signer`.** `lib/identity.Person` holds a `crypto.Signer`, so a hardware key can take the file's place without a change to the callers.
13. **`naructl operator add|revoke`** takes `--admin` (the person file), `--passphrase-file` and, for add, the certificate request by path, and sends the signed request to the socket.
14. **The threat model.** The signer defends the authority keys against orch and against everything that reaches the machine as another user. It does not defend them against its own user or root of its host. That orch's user cannot read the directory is enforced by ownership at deployment (the user `naru-signer`, the directory 0700, orch's user another one) and is stated here as such: the signer checks the directory's mode and owner, and cannot check that the deployment ran orch as another user.

## Accepted risks

- **A signed request can be replayed until it expires.** The expiry is short, and the signer keeps no list of requests it has seen.
- **An operator can revoke any serial, root's included.** The recovery key reissues root; limiting an operator to certificates it issued is not built.
- **A host certificate lasts one year and has no renewal.** It has no subject alternative name.
- **The bundle and the recovery key are the one recovery path.** Losing both loses the authorities.
- **The join is TLS with server authentication, not mutual TLS.** A new signer holds no certificate to present. The binding of root's authorisation to its ephemeral key is what authenticates it: the answer is sealed to that key, so a caller without the key learns nothing from it.
- **The signer's log records issues and revocations,** not the requests' bodies.

## Deferred from aurora#811 A4

`naructl login` with a second factor and short-lived certificates, a re-confirmation of dangerous actions, passkeys, hardware keys and single sign-on are not built. Decisions 11 and 12 are the seam they come in through.

## Consequences

- The authorities and their keys are one process's responsibility, in one directory whose mode the process checks.
- `host/go.mod` and `naructl/go.mod` require `lib/identity`; `naructl/go.mod` moves to Go 1.26.0, which `lib/identity` needs, and requires `golang.org/x/crypto`.
- `lib/identity` gains the signed-request envelope and the wire types the signer and naructl share.
- A deployment must run the signer as its own user and give the socket's group only to the callers that may ask.
- The image does not carry `naru-signer`; it is built and run as a unit of its own until a deployment places it.

## Signs the decision was wrong

- A deployment cannot give orch a user that cannot read the directory: the ownership rule is not the defence it is stated to be, and the keys need a boundary the operating system enforces, such as a hardware key.
- A replayed request is used in an attack: the signer keeps the identifiers of the requests it has seen.
- A caller needs to reach the signer over the network: the socket and the peer listener are not enough.
- An operator's revocation of root is used, or fears of it are: revocation is limited to what the revoker issued.
