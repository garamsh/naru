# Console

What `naructl console` is: the operator's local web console, what it shows of orch, and how it admits a pending host without its key leaving the workstation.

## Current decisions

- **It runs on the operator's workstation**, as `naructl console` in the module `naructl/` (package `naructl/internal/console`). It is not a daemon on the hosts ([ADR 0044](./adr/0044-the-console-is-a-local-web-server-of-naructl-on-the-operators-workstation-that-binds-loopback-guards-its-admit-form-with-a-per-process-token-and-signs-admissions-in-process.md)).
- **Flags.** The ones every naructl command that calls orch takes (`--orch-url`, `--orch-ca`, `--identity`, `--passphrase-file`), and `--listen` (default `127.0.0.1:8484`), and `--admission-ttl` (default 1 h). There is no flag to serve beyond loopback.
- **Pages.** Every page is rendered by the server from orch's answers; nothing is cached.

  | Page | Shows |
  | --- | --- |
  | `/hosts` | admitted hosts: region, labels, address, last seen, and whether the host is draining (read-only: the console has no drain key, `naructl host drain` is the way) |
  | `/microvms` | the microVMs of each host, or of one with `?host=`; empty of data until orch serves them |
  | `/groups` | each group's ready and wanted count, and whether it is stateful; empty of data until orch serves them |
  | `/events` | the newest events of orch's log, newest first |
  | `/pending` | hosts that enrolled and wait, each with the pairing code of its key, and the admit form |

  A page that cannot reach orch answers 502 with the reason. The microVM page shows a host's failure beside the hosts that answered.
- **Admitting.** The operator types the pairing code read from the host's console, a region and labels. The server looks the host up in orch's pending list, compares the typed code with the code of that listed key, builds the admission (the key's full SHA-256, region, labels, expiry at now plus the TTL), signs it with the operator's key in this process and posts it to orch with the typed code, region and labels (`POST /v1/admit`, where orch compares the code against its own key again). The form never carries a key, and a code that does not match, a host orch does not list as pending, an empty region or a malformed label is refused before anything is signed.
- **What guards it.**
  - Loopback only, and the Host header is checked: `console.New` requires `AllowedHosts` and the command lists the listener's own address and `localhost` on its port; an address the command cannot read is an error, not an open guard ([ADR 0067](./adr/0067-naructl-is-a-cobra-command-tree-with-one-flag-name-per-concept-and-the-old-spellings-are-hidden-aliases-removed-in-v0-1-0-dev-13.md)). An address that is not a loopback IP address is refused, since the admit form has no authentication of the browser's. An operator on another machine reaches the console through an SSH tunnel to the workstation that runs it. A request's `Host` must be the server's own address or `localhost` on its port.
  - A per-process token on the admit form, checked on every POST.
  - The key is held by the signing function the command passes in; the server and its pages never touch it.
  - Pages carry a content security policy that allows only the console's own stylesheet and script.
- **orch's client** is `lib/orchapi`'s: hosts, pending, events and admit over TLS 1.3 with the operator's certificate ([orch.md](./orch.md)). The microVMs and groups pages read `GET /v1/microvms` and `GET /v1/groups` through a small interface (`Fleet`); a console built without one says orch does not serve them. orch lists microVMs by asking each admitted host, and reports each one's image digest or, for an OS-form microVM, its boot set, and its network; the microVMs page shows the network and the image in a column each.

- **Install seed** ([ADR 0059](./adr/0059-orch-admits-a-host-that-boots-from-an-install-seed-allocates-its-machine-prefix-and-builds-its-configuration-from-one-fleet-profile.md)). `GET /install-seed` shows a form (lifetime, optional name), and `POST /install-seed` creates an enrollment key valid for that lifetime, builds the NoCloud ISO with the join form (orch's URL and the SHA-256 of the public key of the first certificate in `--orch-ca`, `orchapi.KeyPin` ([ADR 0122](./adr/0122-a-host-renews-its-certificate-under-its-key-in-its-last-90-days-retires-its-others-trusts-a-re-signing-of-an-authority-it-holds-and-pins-orch-by-its-authoritys-key-superseding-adr-0041-and-0082-in-part.md) item 5), the key and the name) and returns it as a download. The key is in the ISO and on no page, since orch keeps only its hash; each download is an `enrollment-key.created` event by the signed-in person. The seed is checked before the key is made, so a refused name costs no key. A console started without those two values answers 501. The pending page shows each host's hardware beside its pairing code: its disks with their partition counts, not a judgement of which is free.

## Rationale

- [ADR 0044](./adr/0044-the-console-is-a-local-web-server-of-naructl-on-the-operators-workstation-that-binds-loopback-guards-its-admit-form-with-a-per-process-token-and-signs-admissions-in-process.md): why the console is local, loopback and signed in process, and the signs it was wrong.
- Plan garamsh/naru#113 decisions 8 and 9: the operator's encrypted person file, and the console on the workstation.

## Open questions

- **Networks.** The console lists no networks: `naructl network list` does. A page of the networks and their members is not decided.
- **A prompt for the passphrase.** The passphrase comes from a file; whether a terminal prompt is wanted waits for operators' use.
- **Actions beyond admit.** The console reads, and admits; revoking a host or changing a group is not built.
