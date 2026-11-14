# ADR 0044: the console is a local web server of naructl on the operator's workstation that binds loopback, guards its admit form with a per-process token and signs admissions in process

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Plan #113 decision 9 gives operators a console. An operator admits a pending host there, and an admission is the one act in naru that adds a machine to the fleet, so how the console is served decides who can cause it. Three questions: where the console runs, what stops a web page that is not the console from posting to it, and where the operator's key is used. The key is encrypted at rest and opened with a passphrase (decision 8); a server on the hosts or beside orch would need it opened there.

## Decision

- **The console is the command `naructl console`**, a web server in the process of the operator's workstation. It is not a daemon on the hosts and not a part of orch. Its pages are embedded in the binary (`embed`), plain HTML, one stylesheet and one script, with no build step.
- **It binds loopback.** `--listen` defaults to `127.0.0.1:8484`, and an address that is not a loopback IP address is refused unless `--allow-remote` is given. On a loopback listener it answers only requests whose `Host` header is its own address or `localhost` on its port, so a page on another origin that resolves its name to loopback cannot use it.
- **The admit form carries a per-process token.** The server makes 32 random bytes at start; every form carries them and a POST without them is refused with 403. The token lives as long as the process and is held nowhere else.
- **It signs in process and never serves the key.** The person file is opened once at start with the passphrase read from `--passphrase-file`. The server is given a function that signs an admission and holds no key; the key is in no page, no response and no request to orch. orch receives the signed statement, which lib/identity's `SignAdmission` produces.
- **It admits what the operator confirmed.** The key an admission names is the one orch lists for the pending host, never one a form carries, and the operator must type that key's pairing code, which the console computes again from the key.
- **It reads orch as the operator.** Calls to orch are `lib/orchapi`'s client over TLS 1.3 with the operator's certificate, with the flags the other naructl commands that call orch take. What orch's API does not carry yet (microVMs, groups) sits behind an interface the console owns.

## Consequences

- A compromised host or orch cannot make the console sign: signing needs the process on the workstation and its open key.
- A process of the operator's user on the workstation can reach the console on loopback and read its pages; it cannot post an admission without the token, which only a page of the console shows it.
- The passphrase is in a file for the life of the command. That file is the operator's to protect, and a prompt is not built.
- A browser on another machine cannot reach the console without `--allow-remote`, which drops the `Host` check.
- Pages are rendered by the server from orch's answers, so they hold no state of their own, and a browser's script does no more than refresh a page and shape a typed code.

## Signs the decision was wrong

- Operators run the console on a bastion and open it remotely, which would show that loopback and a local key do not fit how they work.
- A page of another origin posts an admission, which would show the token or the `Host` check is not enough.
- Operators keep the passphrase file next to the person file, which would show the passphrase protects nothing and a prompt is wanted.
- The pages outgrow plain HTML and a script that refreshes, which would show the build-free form was the wrong one.
