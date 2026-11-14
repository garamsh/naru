# Console

What `naructl console` is: the operator's local web console, what it shows of orch, and how it admits a pending host without its key leaving the workstation.

## Current decisions

- **It runs on the operator's workstation**, as `naructl console` in the module `naructl/` (package `naructl/internal/console`). It is not a daemon on the hosts ([ADR 0044](./adr/0044-the-console-is-a-local-web-server-of-naructl-on-the-operators-workstation-that-binds-loopback-guards-its-admit-form-with-a-per-process-token-and-signs-admissions-in-process.md)).
- **Flags.** The ones every naructl command that calls orch takes (`--orch-url`, `--orch-ca`, `--identity`, `--passphrase-file`), and `--listen` (default `127.0.0.1:8484`), and `--admission-ttl` (default 1 h). There is no flag to serve beyond loopback.
- **Pages.** Every page is rendered by the server from orch's answers; nothing is cached.

  | Page | Shows |
  | --- | --- |
  | `/hosts` | admitted hosts: region, labels, address, last seen |
  | `/microvms` | the microVMs of each host, or of one with `?host=`; empty of data until orch serves them |
  | `/groups` | each group's ready and wanted count, and whether it is stateful; empty of data until orch serves them |
  | `/events` | the newest events of orch's log, newest first |
  | `/pending` | hosts that enrolled and wait, each with the pairing code of its key, and the admit form |

  A page that cannot reach orch answers 502 with the reason. The microVM page shows a host's failure beside the hosts that answered.
- **Admitting.** The operator types the pairing code read from the host's console, a region and labels. The server looks the host up in orch's pending list, compares the typed code with the code of that listed key, builds the admission (the key's full SHA-256, region, labels, expiry at now plus the TTL), signs it with the operator's key in this process and posts it to orch with the typed code, region and labels (`POST /v1/admit`, where orch compares the code against its own key again). The form never carries a key, and a code that does not match, a host orch does not list as pending, an empty region or a malformed label is refused before anything is signed.
- **What guards it.**
  - Loopback only: an address that is not a loopback IP address is refused, since the admit form has no authentication of the browser's. An operator on another machine reaches the console through an SSH tunnel to the workstation that runs it. A request's `Host` must be the server's own address or `localhost` on its port.
  - A per-process token on the admit form, checked on every POST.
  - The key is held by the signing function the command passes in; the server and its pages never touch it.
  - Pages carry a content security policy that allows only the console's own stylesheet and script.
- **orch's client** is `lib/orchapi`'s: hosts, pending, events and admit over TLS 1.3 with the operator's certificate ([orch.md](./orch.md)). orch's API does not carry microVMs or groups yet, so the console reads them through a small interface (`Fleet`) of its own; built without one, the two pages say orch does not serve them.

## Rationale

- [ADR 0044](./adr/0044-the-console-is-a-local-web-server-of-naructl-on-the-operators-workstation-that-binds-loopback-guards-its-admit-form-with-a-per-process-token-and-signs-admissions-in-process.md): why the console is local, loopback and signed in process, and the signs it was wrong.
- Plan garamsh/naru#113 decisions 8 and 9: the operator's encrypted person file, and the console on the workstation.

## Open questions

- **microVMs and groups.** The `Fleet` interface is filled when `lib/orchapi` gains the group types (the groups wiring of garamsh/naru#119) and a way to list a host's microVMs; until then the pages are empty of data.
- **A prompt for the passphrase.** The passphrase comes from a file; whether a terminal prompt is wanted waits for operators' use.
- **Actions beyond admit.** The console reads, and admits; revoking a host or changing a group is not built.
