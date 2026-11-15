# ADR 0061: a host certificate names the host ID and orch's host client accepts the address dialled or the host ID

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#166, part of plan garamsh/naru#113. It adds to [ADR 0059](./0059-orch-admits-a-host-that-boots-from-an-install-seed-allocates-its-machine-prefix-and-builds-its-configuration-from-one-fleet-profile.md) and [ADR 0058](./0058-a-host-boots-from-a-join-form-that-names-an-orch-and-an-enrollment-key-and-init-applies-the-configuration-hostd-brings-back.md), and does not change them.

## Context

- **A host's certificate carried no subject alternative name.** The signer issued CN=hostID and nothing else ([signer.md](../signer.md)). Orch's host client verified the certificate against the address it dialled, so a host that joined from a seed, whose address came from DHCP, could not be reached: `x509: cannot validate certificate for 10.78.0.3 because it doesn't contain any IP SANs`. The first QEMU scenario of a fleet installed from one seed found it at the last step, the microVM.
- **A joined host's address can change** while its certificate lives a year, so an IP name would go stale.
- **The host ID is the name orch and the signer already agree on.** It is in the signed admission statement, which the signer verifies, and it is not in the certificate request, which the host wrote.
- **A control host's certificate** is the orch or Node certificate the operator issued and names the address; those hosts must work unchanged.
- **A joined host needs no registry.** `lib/layout` emitted the `registry` service for every Node, and `naru-registry` is a test-only binary the QEMU fixture stages into the inputs tree. A host that booted from a seed has no inputs tree, so the service restarted for ever.

## Decision

**A host certificate carries the DNS name that is the host ID, from the signed statement, and no IP address. Orch accepts a host's certificate when it chains to the host authority and names the address orch dialled or the host ID.**

1. **The signer's `IssueHost` gives the certificate the DNS name `adm.HostID`**, the host ID of the verified admission statement. The names of the certificate request are not read, so a host cannot ask for another host's name or for an address. The certificate carries no IP address.
2. **Orch's host client verifies the chain itself** (`VerifyConnection`, with the host authority as the root and server authentication as the usage) and then accepts the certificate that names either the address dialled or the host ID. It is one rule for every host: a control host's certificate names its address and a joined host's names its ID, and the inventory carries no flag that says which a host is. A certificate that names another host's ID, or neither name, or chains to another authority, is refused. The client is built once per host and address and cached.
3. **The smaller design.** The alternative, to set the TLS server name to the host ID for a joined host and keep the address for a control host, needs a record of which is which and a second path in the client; this one has a single check and no new field.
4. **The Node a host's configuration builds (`HostOnly`) runs no registry service.** The services of a joined host are `mvmd` and `naru-hostd`; it pulls images from the registries its profile names.
5. **`GET /v1/hosts` carries each host's `machinePrefix`** (omitted when none was allocated), which `lib/orchapi`'s `Host` gives and `naructl host list` shows. Events carry no prefix.
6. **naructl reads the authorities.** `naructl signer ca` (`--socket`, or `--config` and `--node` through the Node API's `/v1/signer/ca`) prints the signer's authorities, each as the line `# <name> authority, SHA-256 <hex of its DER>` and the PEM; `--authority` narrows it. `naructl orch ca` prints the orch authority alone. Orch serves no authority of its own and no route is added, so `orch ca` reads the authority orch's certificate chains to from the signer. `naructl host list` shows each host, its address, Machine prefix, last report and labels.

## Consequences

- **Orch's `spec.orch.hostCA` is a bundle.** A control host's own API certificate chains to the authority `naructl gen host-ca` writes, and a joined host's to the signer's host authority; orch verifies the chain against `hostCA` (`--host-ca`), so the operator gives it both, the second from `naructl signer ca`. The first scenario of a fleet installed from one seed found it: the chain was never reached while the name check came first.
- **A host certificate issued before this change carries no name** and is accepted only by address, as before; a joined host's certificate is issued at admission, so every joined host has the name.
- **Renewal of a host certificate** stays open ([signer.md](../signer.md), Open questions); a renewal issues the same name.
- **The check is in orch, not in `crypto/tls`'s name verification**, so a change to what a certificate may name is a change to `orch/internal/hostclient` and its tests.
- **The operator reads the CA hash with a command**, not with `curl` and `openssl`.

## Signs the decision was wrong

- A host reachable by two addresses, or behind a name of its own, which would say the host ID is not the one name and the certificate wants more than one.
- A control host whose certificate names its ID and not its address, which would make the address branch dead and the check a name check on the ID alone.
