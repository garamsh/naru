# lib/identity

What naru's authorities and their callers agree on, in pure Go with the standard library's crypto (plus scrypt from `golang.org/x/crypto`).

- Roles `root`, `operator`, `reader`, `automation`, `host`, `orch` and `exposer` (a cloud controller manager, its Subject `exposer.<network>`, read with `ExposerNetwork`), carried in a certificate's one OrganizationalUnit: `RoleOf`, and `Authenticate`, which turns verified chains and a revocation list into an `Identity`.
- The revocation list, signed JSON from the signer's Ed25519 revocation key: `SignCRL`, `VerifyCRL`, `CRL.CheckSeq`, `CRL.CheckChain`, and the key's PEM: `ParseRevocationPublic`, `EncodeRevocationPublic`.
- `RevocationList`, the list a process checks certificates against: fetched from a `CRLSource` (`SocketSource` for the local signer's socket), refreshed after a TTL, kept on disk for a restart within `MaxAge` (`DefaultMaxAge`, 10 minutes), never replaced by a lower seq, and failing closed with `ErrNoRevocationList` or `ErrStaleRevocationList`. `Check` and `VerifyConnection` are what a server's `tls.Config` calls for its clients.
- `PeerCheck`, the one verification a naru TLS client makes of a server: the chain, the name rule of ADR 0061 and, when it has a `RevocationList`, the revocation check (ADR 0062). `lib/orchapi`'s `PinCA` and orch's host client both call it.
- `PairingCode`: 40 bits of the SHA-256 of a public key's DER, `XXXX-XXXX`.
- The admission statement an operator signs for a host: `SignAdmission`, `VerifyAdmission`.
- The person file and the recovery key file, each encrypted by a passphrase (scrypt N=2^15, r=8, p=1; AES-256-GCM): `EncryptPerson`, `DecryptPerson`, `GenerateRecovery`, `DecryptRecovery`.
- The signed request a caller sends the signer, and the wire types the signer and naructl share: `SignRequest`, `VerifyRequest` (the action, an expiry and a body, signed with the caller's key and certificate).
- The request the recovery key signs for a new root, with no certificate and a nonce: `SignRecoveryRequest`, `VerifyRecoveryRequest`, `NewNonce`.
- The sealed box, X25519 with HKDF-SHA256 and AES-256-GCM: `Seal`, `Open`.

Linked by `host` (the signer) and `naructl`, and by every unit that checks a caller's role.
