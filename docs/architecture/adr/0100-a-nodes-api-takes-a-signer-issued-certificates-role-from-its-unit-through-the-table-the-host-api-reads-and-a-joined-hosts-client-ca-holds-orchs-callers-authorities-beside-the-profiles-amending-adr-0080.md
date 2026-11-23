# ADR 0100: A Node's API takes a signer-issued certificate's role from its unit through the table the host API reads, and a joined host's client CA holds orch's callers' authorities beside the profile's, amending ADR 0080

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#349, part of goal garamsh/naru#262. It amends [ADR 0080](./0080-a-seed-joined-host-accepts-an-operator-certificate-the-signer-issued-and-takes-its-role-from-the-certificates-organizational-unit.md) item 3 (where the authority comes from) and extends its items 2 and 4 from the host API to the Node API. It changes [init.md](../init.md)'s roles, [host.md](../host.md)'s role table and [config.md](../config.md)'s `naructl` callers.

## Context

- **A joined host's Node API refused the operator's certificate.** Found on aurora while upgrading a host that joined from an install seed (garamsh/naru#349): the Node API answered 403 to the person file's certificate and did not trust the `gen api-client` file.
- **The Node API reads the Organization only.** `roleOf` in `lib/init/api/roles.go` maps `naru:admin` and `naru:reader`. A certificate the signer issues to a person has no Organization; its role is its one OrganizationalUnit (`lib/identity`). The host API learned that table in ADR 0080; the Node API was left out, so an operator certificate that verified got 403.
- **A joined host's Node API trusts one field.** Its `spec.api.clientCA` is the profile's optional `apiClientCA` (`lib/layout`, `naructl gen host-profile --api-client-ca`). The authority a host's hostd trusts for its callers (orch's callers' bundle, which holds the operator authority beside orch's, ADR 0080 item 3) is not in it, and a profile without the field gives the host no Node API at all.
- **Upgrading a host is a Node API call.** `POST /v1/upgrade` needs the admin role ([init.md](../init.md)), so upgrade-a-host fails on a joined host for the operator who holds only the signer's person file.

## Decision

**A Node's API gives a certificate the signer issued the access its unit gives, through the one table the host API reads, and the client CA of a joined host is the bundle of the profile's authority and the authorities orch's callers chain to.**

1. **One table.** `identity.PersonAccess` (`lib/identity`) maps a certificate's one OrganizationalUnit: `root` and `operator` write, `reader` and `automation` read, and `host`, `orch`, an unknown unit, more than one unit or none give nothing. The host API's `roleOf` reads it for `provisioner` and `reader`, as ADR 0080 item 2 decided, and keeps `orch` as a provisioner on its own; the Node API's `roleOf` reads it for `naru:admin` and `naru:reader`. The table is written once.
2. **The Organizations stay.** `naru:admin` and `naru:reader` on the Node API, and `naru:host-provisioner` and `naru:host-reader` on the host API, are read as before; the role of a certificate is the highest of what its Organizations and its unit give.
3. **The trust set is data.** `spec.api.clientCA` is already a PEM bundle, and the API verifies against all of it. A joined host's `spec.api.clientCA` is the profile's `apiClientCA` (when it has one) followed by the authority bundle orch's callers chain to (`JoinedHost.OrchCA`, ADR 0080 item 3). `lib/layout` builds it in the stage that builds the host's configuration; the API gets no special case for a joined host, and a profile without `apiClientCA` now gives the host a Node API trusting orch's callers' authorities.
4. **`naructl` Node API commands take `--identity`.** Every command that took `--config` for the Node API takes, as the alternative, `--identity <person file> --passphrase-file <file> --host-ca <authority PEM> --endpoint <address> [--server-name <h-id>]` as the host commands do (ADR 0080 item 4), through one code path shared with them.

## Options considered

- **Carry the API authority in the profile of every joined host (`apiClientCA` always).** The operator would need that authority's key for each joined host, which ADR 0080 removed for the host API; it gives the person file nothing.
- **Give a joined host's Node API a second, separate listener for operators.** A second surface to keep; the first's trust set is already data.
- **Copy the table into `lib/init/api`.** Two tables drift; the host API and the Node API are told the same thing about the same certificates.

## Consequences

- **An operator reads, applies and upgrades a joined host with the certificate they already hold.** Revoking it (`naructl operator revoke`) ends it on the Node API as on the host API, since the Node API checks the revocation list.
- **The signer's operator and root become admin on every Node API that trusts the operator authority.** On a joined host they were already provisioners of its host API, which controls the host and its Nodes; a Node that does not trust the operator authority is unchanged.
- **`automation` reads and cannot upgrade**, as on the host API.
- **A joined host booted before this change keeps its stored configuration** until it is admitted again or its profile is applied: the new trust set is built when orch builds the configuration.
- **A deployment whose callers' bundle lacks the operator authority gets a handshake refusal**, as ADR 0080 said for the host API.

## Signs the decision was wrong

- Tools that must read a Node but never write need a role below `automation`, or one that writes only a part: the table would then need more than two levels.
- A joined host needs a Node API credential that is not under orch's callers' authorities: the trust set would then need its own field in the profile beside `apiClientCA`.
