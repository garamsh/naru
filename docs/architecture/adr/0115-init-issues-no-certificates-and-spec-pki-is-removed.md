# ADR 0115: init issues no certificates, and spec.pki is removed

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for goal garamsh/naru#375, goal 2 of garamsh/naru#373, in its fourth pull request. It supersedes [ADR 0013](./0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md) and amends [ADR 0065](./0065-a-step-of-the-clock-has-init-judge-every-certificate-against-the-new-time-and-a-step-of-an-hour-or-more-is-refused-once-the-clock-has-been-set.md) items 1 to 3, [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) items 3 and 7, where `spec.pki` is a keyed list and has rows of the field classes, [ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) item 7's `spec.pki.authorities[].key` and [ADR 0058](./0058-a-host-boots-from-a-join-form-that-names-an-orch-and-an-enrollment-key-and-init-applies-the-configuration-hostd-brings-back.md) item 2's lease address for `spec.pki`; their bodies are not edited.

## Context

- **`spec.pki` had one producer, and it is gone.** gaon's control-plane Node was the only configuration that carried an authority in `spec.pki` (ADR 0013 item 10, ADR 0024 item 2), and [ADR 0114](./0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md) removes it. Since then no layout writes an entry, and nothing else writes the field but init's store: the field had no `omitempty`, so every revision init stores holds `pki:` with two empty lists.
- **Nothing on main reads an issued certificate.** The readers of `spec.pki` are init's own packages (`lib/init/config`, `lib/init/pki`, `lib/init/boot`, `lib/init/apply`, `lib/init/run`), and the only test that gives it an entry is the QEMU time test, which makes a throwaway authority to have something issued after the clock steps.
- **Goal 3 does not name it.** ADRs 0108 to 0113, the design of the Machine service, give every certificate a Machine or a host needs from `naru-signer` (ADR 0040) and name no `spec.pki`.
- **The coordinator of #375 decided on 2026-11-24** that `spec.pki` is removed unless something on main reads it or goal 3 names it: dead configuration is cruft.
- **The field is in every host's store.** A store written before this change holds `pki: {authorities: [], certificates: []}`, and init parses it strictly, so a field that is simply deleted makes every host refuse its stored configuration after the upgrade and roll back (ADR 0025).

## Decision

**Init issues no certificate. `spec.pki`, the issuer and its renewal are removed, and a configuration written before then still loads.**

1. **The field is retired.** `spec.pki` stays a known field of `naru.yaml` so that a stored configuration parses. Two empty lists, or a missing field, are read as no field, and the configuration is the same as one without it; an entry in either list is refused at `spec.pki`, in the full form and in the join form. A configuration init writes back omits the field.
2. **The issuer goes.** `lib/init/pki`, the boot step that issued before the services started, the renewal loop init ran for as long as the Node was up, and the apply engine's `issue-certificate` and `forget-certificate` actions are removed. `spec.pki` is no longer a keyed list of a patch, a reboot-only field, or a place of the redaction table.
3. **The clock no longer reports its steps.** `timesync.Syncer.OnStep` and `linux.Host.OnTimeStep` existed to tell the issuer (ADR 0065 item 1); they go with it. ADR 0065 items 4 and 5 (the one-hour bound once the clock has been set, and a reboot as the cure) stand.
4. **The network no longer reports the Node's addresses.** `netcfg.Applied.Addresses` was read only to name a certificate with `nodeAddresses` (ADR 0058 item 2's one accessor); it goes. The resolvers and the lease's time servers stay in `Applied`.
5. **The judge's issuing check goes.** `judge.Issuance` is removed, and `judge.TimeSynced` asks for the step before the first service starts instead of before `authority written`. The QEMU time test boots without a `spec.pki`.

## Options considered

- **Keep `spec.pki` working with no producer.** Every change to init's boot, apply and clock would keep carrying an issuer nobody configures, kept working by unit tests alone (simplicity.md, "Delete, don't wrap"); history keeps it for a later producer.
- **Delete the field outright.** Every host's store holds `pki:` with two empty lists, so every host would refuse its stored configuration after the upgrade and roll back.
- **Keep the field and ignore its entries.** A configuration that asks for a certificate would boot without it and fail later where the certificate is read; a refusal names the field at the boot that meets it.

## Consequences

- A host that upgrades keeps its stored configuration: its empty `spec.pki` is read as no field, and the next revision init stores leaves it out.
- A configuration that carries an authority or a certificate in `spec.pki` is refused at boot and at apply. No layout on main writes one.
- A step of the clock is logged and shown by `GET /v1/time` as before, and changes nothing else.
- The QEMU time test no longer shows that a certificate issued after the step is valid; it shows the step before the first service starts.

## Signs the decision was wrong

- A Machine or a host needs a certificate made on the Node at boot, from an authority its configuration carries, that `naru-signer` cannot give: init issuing would be designed again under goal 3, not restored from history as it was.
- A host refuses its stored configuration after an upgrade at `spec.pki`: a layout wrote an entry this ADR did not find.
