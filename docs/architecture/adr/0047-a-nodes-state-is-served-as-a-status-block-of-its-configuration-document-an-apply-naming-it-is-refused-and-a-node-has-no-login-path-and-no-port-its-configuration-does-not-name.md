# ADR 0047: a Node's state is served as a status block of its configuration document, an apply naming it is refused, and a Node has no login path and no port its configuration does not name

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#109 (phase of #113). It extends [ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) and [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) by reference: their bodies are not edited, and what they say holds with this decision added.

## Context

- **A Node's state is spread over three calls.** `GET /v1/version` gives the version, boot, slot and last upgrade, `GET /v1/services` the services, and `GET /v1/config` the revision and the configuration. An operator who wants to know what a Node is and what it is running reads all three.
- **The configuration is declared and the state is observed.** A document that carries both must not let a round trip turn an observation into a declaration: `naructl get config`, edited and applied, would otherwise send the Node its own version back.
- **`config.Parse` already refuses a key it does not know,** so a `status` in a configuration fails today, but as an unknown field of a document and not as a refusal that says why.
- **The Node has no shell, no getty, no login and no SSH server,** and init holds the console without reading it. That is a property of the image that nothing states as a test, so a later change that adds a way in would pass every test.
- **The host cannot look inside a Node.** A test on the host reads the Node's console and its API, not its `/proc` or its file system.

## Decision

1. **`GET /v1/config` returns the configuration as a document with a top-level `status` beside `spec`,** inside the same `{revision, config}` body as before. `status` has `version`, `bootId`, `slot`, `lastUpgrade` (absent until an upgrade has ended), `revision`, and `services`, each `{name, state, restarts}`. Each value is the one `GET /v1/version` and `GET /v1/services` give, read by the same code, so the document and the two calls cannot disagree. `services` leaves out `pid` and `startedAt`, which change at every restart and say nothing about the Node's state. A boot ID that cannot be read is 500, as for `GET /v1/version`.
2. **`status` is never declared.** It is in no stored configuration and no revision. An apply whose patch or `config` has the key `status` is 422 with `fields: ["status"]` and the message that status is observed, not declared, and is refused before the apply engine is consulted, so nothing is stored or started by a request that names it.
3. **`naructl get config` prints that one document as YAML:** the configuration redacted as before and the status, with `status.version` equal to what `naructl version` prints. It no longer prints the `{revision, config}` envelope; the revision is `status.revision`, and `naructl apply` still reads the revision from the Node, not from the output.
4. **A Node has no login path, and the QEMU tests state it.** The image carries no shell, getty, login or SSH server, no process other than PID 1 holds a terminal, nothing reads the console, and the Node listens on the TCP ports its configuration names and no others. A test service, `naru-loginprobe`, runs on the Node and reports on the console the listeners in `/proc/net/tcp` and `/proc/net/tcp6`, the files under `/` (outside `/proc`, `/sys`, `/dev`, `/run` and the data disk, where tests stage their programs) that are or link to a program named `sh`, `ash`, `bash`, `dash`, `zsh`, `busybox`, `getty`, `agetty`, `mingetty`, `login`, `sshd` or `dropbear`, the processes other than PID 1 that hold `/dev/console`, a tty or a pty, and the threads blocked in a read of one. The test requires none of the last three and every listener to be in the list below.
5. **The listening ports of a control-plane Node that runs gaon with the Node API,** the Node of the test, are:
   - 50000: the Node API (`spec.api.listen`, by default `0.0.0.0:50000`).
   - 6443: gaon's API server.
   - 2379 and 2380: etcd's client and peer ports.
   - 5000: the registry service, on the loopback.
   - mvmd listens on a Unix socket, and gaon's scheduler, controllers and Node Agent open no port with the flags `naructl` gives them, so no other port is open. A Node with more configuration has the ports that configuration names, such as hostd's with `spec.host`, and a change to the layout that opens a port changes this list in the same pull request.

## Consequences

- An operator reads a Node's state with one command, and the output is a document that cannot be applied back by mistake: the apply says why.
- A client that read `revision` from the top of the `GET /v1/config` body is not affected; one that read the configuration from `naructl get config`'s old JSON output reads YAML now, and the revision in it is `status.revision`.
- The test adds a service to the Node under test and one more program to the QEMU tests' inputs; the image is unchanged.
- The ports list is a check that follows the layout: a service added to the layout that listens fails the test until the list names it.
- The test's reading of "nothing reads the console" is that no process other than init holds a terminal and no thread is blocked reading one, at the moments the probe looks. It cannot see a reader that opens a terminal between two passes, and it does not read the kernel's own console handling.

## Signs the decision was wrong

- An operator needs the status without the configuration, or the configuration without the status, often enough that a flag or a second document earns its place.
- A client puts `status` into an apply on purpose, to assert a precondition: the refusal is replaced by a check that the observed value is the one named.
- The probe's list of program names misses a way in an image grows, or finds a name a legitimate service uses, and the list needs an allowance: the test then names the file it allows.
- A Node's ports differ by configuration so much that one list is not what a reader can check: the list moves to the layout, which derives it.
