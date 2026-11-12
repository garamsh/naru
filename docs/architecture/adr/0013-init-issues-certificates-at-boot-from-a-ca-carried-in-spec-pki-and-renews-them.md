# ADR 0013: init issues certificates at boot from a CA carried in spec.pki and renews them

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#35, part of goal garamsh/naru#34. It supersedes item 5 of [ADR 0012](./0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md) and no other item of it: ADR 0012's items 1 to 4 and 6 stand, and item 5's file layout under `/run/gaon` stands as the path the QEMU tests choose.

## Context

- **ADR 0012 item 5 has the test tooling issue every certificate and naru carry the files.** naru holds no CA and issues nothing. The files, keys included, are `spec.files` content in each Node's NoCloud drive.
- **gaon's end state is that the Node issues what it serves with.** Goal #34 moves the issuing of a Node's certificates from the tooling that builds the configuration to init, so a configuration carries a CA and the Node makes its own keys.
- **A key made on the Node never leaves it.** A key in the configuration is in the clear in the drive and in every copy of the configuration; a key made at boot is on the tmpfs and nowhere else.
- **init is PID 1 and supervises the services** ([init.md](../init.md)). It can restart one by name.
- **A certificate expires.** Every certificate has a validity, and a Node that boots once and runs for longer than it must replace the certificate, and the services that read it, without a reboot.

## Decision

**`spec.pki` carries certificate authorities and the certificates init issues from them after the network is configured and before any service starts, with a new key each time, and init renews each certificate when two thirds of its validity has passed and restarts the services it names.**

1. **The field.** `spec.pki.authorities` lists the authorities (`name`, `certificate` in PEM, optional `key` in PEM, `certificatePath`, optional `keyPath`) and `spec.pki.certificates` lists the certificates (`name`, `authority`, `certificatePath`, `keyPath`, `commonName`, optional `organization`, `usage` of `server` or `client`, `dnsNames`, `ipAddresses`, `nodeAddresses`, `validity` and `restart`). The fields are in [config.md](../config.md).
2. **Validation.** The configuration is refused, naming the field, when an authority's key is missing for an authority a certificate names, when a key does not match its certificate, when an authority's certificate is not a CA, when `keyPath` is set on an authority with no `key`, when a path is outside `/run` or inside `/run/naru`, when a path is also in `spec.files` or named twice in `spec.pki`, when `restart` names a service that is not in `spec.services`, and for an unknown `usage`, an invalid address, a `validity` outside 1 hour to 87600 hours, an empty `commonName`, a duplicate name, and a server certificate with no `dnsNames`, `ipAddresses` or `nodeAddresses`.
3. **Names.** A certificate's names are its `dnsNames` and `ipAddresses`; with `nodeAddresses` they also include each address of the Node's `network-config`, and a server certificate with `nodeAddresses` also gets `127.0.0.1`.
4. **Keys.** Each key is ECDSA on P-256, generated on the Node at every boot and at every renewal, written mode `0600` under `/run`, which is a tmpfs, and never to the data disk. An authority's key is written, mode `0600`, only where the authority names a `keyPath`, for a service that signs with it; otherwise it is used in memory and not written. The authority's certificate and the issued certificates are mode `0644`.
5. **Validity.** A certificate is valid for its `validity` (1 hour to 87600 hours, 720 hours when it names none) from the time init issues it; its start is one hour earlier, so a peer whose clock is behind accepts it. Its end is capped at the end of its authority's certificate, and init logs `authority expires soon` with the name, the capped expiry and the certificate when the cap applies. An issue whose capped life is under one hour fails, at boot and at a renewal. An authority that is not valid at the time of issue makes the issue fail. The times are the Node's wall clock at boot, which on Proxmox and QEMU comes from the emulated RTC: a clock before the authority's start refuses the boot, and a clock that steps later moves the renewal times with it.
6. **Boot.** Issuing is a step of init's boot sequence, after the network is configured and the files are written and before any service starts. A failure to issue refuses the boot as a failure of the other steps does: no service starts. After any refusal init still renews the certificates it issued, for the services that started before it.
7. **Renewal.** init renews a certificate when two thirds of the life it was issued with (to its capped end, not its `validity`) has passed: it generates a new key, signs a new certificate, writes both beside the old files, links the old key aside, renames the new key and then the new certificate over the old ones, and puts the old key back if the certificate rename fails. After the pass it restarts each service named in `restart` once, however many certificates name it. A service that is not running, such as a control-plane service on a Node that is not the control plane, is not started by a restart. A renewal that fails is logged and retried after one minute, and no service is restarted until it succeeds.
8. **Log lines.** init logs `authority written` for each authority, `certificate issued` for each certificate with its name, authority, usage, path and expiry, `certificate renewed` with the same fields, and `certificate renewal failed` with the error, and `authority expires soon` when a certificate's end is capped. It logs no key.
9. **The judge.** `naru-judge` judges the issuing only with `-pki` (`JUDGE_PKI=1` of `make judge-serial`), and takes node-1's interface count from `-node1-interfaces`, so a log that shows no issuing fails when `-pki` is given.
10. **The QEMU tests.** The control-plane Node of the test that runs gaon from `spec.pki` carries the CA and its key in its configuration only, and gets the API server's, scheduler's, controllers' and its own agent's pairs from init. The worker Node carries an operator agent pair in `spec.files`, and another test runs every Node, the control plane included, with operator files, as before.

## Consequences

- **A configuration that carries a CA key can issue for its Node,** and a configuration that carries only files still works unchanged; `spec.pki` is optional.
- **The CA's key is in the NoCloud drive of every Node that issues** in the clear, as `spec.files` content was. A Node that holds the CA key can issue a certificate for any name the CA is trusted for, so the key is only as private as the drive. Who holds it, and whether one CA signs for more than one Node, is the operator's.
- **A Node's certificates and keys change at every boot,** so a peer that pinned one fails after a reboot; peers trust the CA, not the leaf.
- **A renewal restarts the services it names,** so a renewal is a brief outage of each, and a service that reads its certificate on every handshake needs no restart but is restarted all the same.
- **The key and the certificate of a renewal are replaced in two renames,** not as one step. A reader that opens both between them sees a new key and the old certificate, and its handshake fails until the second rename; a service that is restarted afterwards reads both. A crash between the renames leaves the new key beside the old certificate. When the certificate rename fails init restores the old key, and when that fails too the error says so.
- **The renewal times follow the Node's wall clock.** A certificate's life is capped by its authority's, so a Node whose authority ends soon issues short-lived certificates or refuses.
- **Init's dependencies do not change:** the issuing uses the standard library's `crypto/x509` and `crypto/ecdsa`.
- **The test tooling still builds the CA,** and no longer the leaf certificates of a Node that uses `spec.pki`; the test host's own client certificate is still the tooling's.

## Signs the decision was wrong

- A peer that reads a certificate between the two renames of a renewal and fails: the pair needs one rename, as one file or a directory swapped whole.
- A certificate that expires on a Node that has been up for longer than its validity: the renewal did not run, or kept failing, and the retry is the wrong form.
- A service that does not pick up the renewed pair after its restart: `restart` is the wrong hook, and the service needs a reload signal.
- A need for a certificate with another key type, or an authority that is not in the configuration: the field's shape is wrong, and this item is revisited.
- A need for the CA's key to leave the configuration, in a signing service the Node asks: `spec.pki` becomes the bootstrap and this decision narrows to it.
- Two Nodes of one cluster that cannot hold the same CA key without trusting each other: one CA per Node, or a signing service, replaces the shared key.
