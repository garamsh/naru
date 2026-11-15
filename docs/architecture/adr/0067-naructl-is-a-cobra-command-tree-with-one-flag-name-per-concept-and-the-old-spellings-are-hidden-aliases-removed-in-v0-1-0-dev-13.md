# ADR 0067: naructl is a cobra command tree with one flag name per concept, and the old spellings are hidden aliases removed in v0.1.0-dev.13

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#190, part of garamsh/naru#197. It amends [ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md), [ADR 0028](./0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md) and [ADR 0057](./0057-an-operator-reaches-the-signer-through-the-node-apis-admin-role-which-forwards-v1-signer-to-the-signers-unix-socket-and-adds-no-authority.md) in the names of the flags they give naructl, and does not edit them.

## Context

- **Every command parsed its own arguments.** A hand-written dispatcher chose the command and each command ran `flag.FlagSet` on the rest. `-h` ended with "flag: help requested" and exit 1, the usage text was written by hand and left out `network apply|get|list|delete` and `signer reissue-root`, a parse error was printed twice, and the parser for flags after positional arguments parsed again what followed `--`.
- **One concept had several names.** The CA's directory was `--ca` in `gen client`, `gen token`, `gen node` and `gen kubernetes*`, `--ca-dir` in `gen api-ca`, `gen host-ca` and the client generators, and `--out` in `gen ca`. The address of the Node API was `--node` and of the host API `--host`, while `gen api-client` and `gen host-client` took `--endpoint`.
- **`genNode` was one function of 272 lines.** The three `gen *ca` commands each carried a copy of the same check and write.
- **The console's Host guard could be switched off by a failure.** `loopbackHosts` returned nil when it could not read its listener's address, and the console treated an empty `AllowedHosts` as "allow any".

## Decision

**naructl is a tree of cobra commands. A concept has one flag name, `--ca-dir` for an authority's directory and `--endpoint` for the address of the API a command calls. The old spellings stay as hidden aliases that print one line to stderr, and stop working in v0.1.0-dev.13.**

1. **The framework is `github.com/spf13/cobra`** (with `pflag`, which it uses, and `mousetrap`, which it needs on Windows only). The tree is built in `naructl/internal/command/tree.go`, and `command.Run` builds it afresh for each run. Usage, `-h` and `completion bash|zsh|fish|powershell` are generated; `-h` exits 0. A parse error is returned once and `main` prints it once.
2. **`--ca-dir`** names the directory that holds an authority, in every `gen` command that reads or writes one, `gen ca` among them. **`--endpoint`** names the address (`ip[:port]`) of the Node API, the host API or the signer through the Node API, in every command that calls one. `gen api-client` and `gen host-client` keep `--endpoint` as the address written into the client file.
3. **The old names are aliases for one release.** `--ca` (`gen client`, `gen etcd-client`, `gen token`, `gen node`, `gen kubernetes`, `gen kubernetes-node`), `--out` of `gen ca`, `--node` (Node API and signer commands) and `--host` (host API commands) are hidden from usage and completion, hold the same value as the flag they stand for, and count as it when a flag is required or refused. A command line that uses one prints `naructl: --old is deprecated, use --new (it stops working in v0.1.0-dev.13)` to stderr and runs. **v0.1.0-dev.13 removes them.** The repository does not use them: documents, `tooling/qemu` and the tests give the new names, and a test fails when a document shows a flag that is hidden or does not exist.
4. **`gen ca`, `gen api-ca` and `gen host-ca` are one implementation**, which differs by the subdirectory and the noun in its messages. Each refuses when the directory already holds that authority's certificate or key.
5. **`console.New` requires `AllowedHosts`**, and `loopbackHosts` returns an error where it cannot read the listener's address. A server with no allowed Host is not made.

## Consequences

- A flag a command lacks, a missing subcommand and `-h` behave the same in every command, and shell completion needs no code of ours.
- An operator's script that gives `--ca`, `--node`, `--host` or `gen ca --out` keeps working for one release and learns from stderr what to change. The release notes of the release that carries this name the five spellings.
- naructl depends on three modules more, and a vulnerability in cobra or pflag is naructl's. The module builds from `go.sum`, and the vulnerability scan of the module covers them.
- The positional rules (`logs <service>`, `host admit <id>`) are checked by each command, not by cobra, so a command that forgets one runs without it.
- Output on stdout is unchanged. The messages of an error differ where a command's `flag` text was replaced by pflag's ("unknown flag: --x" for "flag provided but not defined: -x").

## Signs the decision was wrong

- Scripts still use the aliases at v0.1.0-dev.13: a release was too short, and the removal moves by a new ADR.
- Two concepts end up sharing a flag name where the same spelling means different things (`--endpoint` is already the address in a client file and the address a command calls): the rule should be one name per meaning, and one of the two is renamed.
- A cobra release changes how a hidden flag or an argument after `--` is parsed and a test of this decision fails on a bump: the framework costs more than the hand-written parser did.
