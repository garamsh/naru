# lib/identity

What naru's authorities and their callers agree on, in pure Go with the standard library's crypto (plus scrypt from `golang.org/x/crypto`).

- Roles `root`, `operator`, `reader`, `automation`, `host` and `orch`, carried in a certificate's one OrganizationalUnit: `RoleOf`, and `Authenticate`, which turns verified chains and a revocation list into an `Identity`.
- The revocation list, signed JSON from the signer's Ed25519 revocation key: `SignCRL`, `VerifyCRL`, `CRL.CheckSeq`.
- `PairingCode`: 40 bits of the SHA-256 of a public key's DER, `XXXX-XXXX`.
- The admission statement an operator signs for a host: `SignAdmission`, `VerifyAdmission`.
- The person file and the recovery key file, each encrypted by a passphrase (scrypt N=2^15, r=8, p=1; AES-256-GCM): `EncryptPerson`, `DecryptPerson`, `GenerateRecovery`, `DecryptRecovery`.
- The sealed box, X25519 with HKDF-SHA256 and AES-256-GCM: `Seal`, `Open`.

Linked by `host` (the signer) and `naructl`, and by every unit that checks a caller's role.
