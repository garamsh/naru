# ADR 0095: the signer lists and revokes every serial it holds for a host ID, so a lost host's certificate is revoked without its serial

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#331, part of goal garamsh/naru#262. It adds to [ADR 0077](./0077-the-signer-rotates-a-hosts-pair-with-the-old-serial-revoked-registers-offline-pairs-by-serial-and-hostd-refuses-a-revoked-key.md), whose registry of serials by host ID it reads, and changes no rule of it.

## Context

- **A lost host cannot report its serial.** `naructl operator revoke --serial` takes a serial, orch's records carry none, and the host's API is gone. The operator had to find the serial in a certificate file they kept or in the signer's console line, which init re-prints only for the newest 16 KiB ([ADR 0036](./0036-a-re-print-carries-at-most-the-newest-16-kib-of-the-console-log-and-the-api-servers-liveness-probe-is-a-tls-request-that-it-can-answer.md)).
- **The signer already holds the serials by host ID.** ADR 0077 has it record every serial it issues, or is told of, for a host, and `RetireHost` revokes the ones that are not kept.
- **Revoking serial by serial from a list is two steps the operator can get half right,** and each revoke signs the list again.

## Decision

1. **`POST /v1/lookup-host` (root or an operator; body the host ID) answers every serial the signer holds for the host, each with whether the revocation list carries it.** It changes nothing.
2. **`POST /v1/revoke-host` (root or an operator; body the host ID) puts every serial of the host that is not on the list on it, in one signing and one commit, and answers the same list as it then stands.** The host keeps its record, so a retry, or a serial already revoked, changes nothing and publishes no sequence. A later `rotate-host` of the host still works, since `RetireHost` skips serials already revoked.
3. **A host ID the signer holds no serial for is 409 `the signer holds no certificate for this host`, and the message names `naructl signer register-host`,** the call that records the serial of an offline pair. A host ID that is not a plain name is 400.
4. **`naructl signer lookup-host --host-id` and `naructl signer revoke-host --host-id`** sign these as `register-host` signs its call (`--admin`, `--passphrase-file`, `--socket` or `--config` and `--endpoint`) and print each serial with `valid` or `revoked`. The lookup is for an operator who wants to revoke one serial with `naructl operator revoke --serial`.
5. **`docs/operate/recover-a-member-or-host.md` part 2 step 3 uses `revoke-host`** and no longer sends the operator to read a serial from a kept certificate.

## Consequences

- **A lost host is revoked from its ID alone,** with one command and one list sequence, for every serial the signer holds for it, a rotation's unused one included.
- **A host the signer never recorded still needs `register-host` first,** with the serial from a certificate the operator kept. That is the same limit ADR 0077 states for rotation.
- **The revocation is the host's whole record,** not a chosen serial: an operator who wants one serial revoked uses `lookup-host` and `operator revoke`.
- **Two calls are added to the signer's table of roles:** root and an operator, as for `register-host`. They add no authority, since both could revoke any serial.

## Signs the decision was wrong

- Operators revoke a host whose certificate they wanted to keep, because the call takes the whole host: `revoke-host` should take a serial or a confirmation.
- Hosts are lost whose serials the signer never recorded often enough that `register-host` is the usual first step: `gen node` should register the serial it makes.
