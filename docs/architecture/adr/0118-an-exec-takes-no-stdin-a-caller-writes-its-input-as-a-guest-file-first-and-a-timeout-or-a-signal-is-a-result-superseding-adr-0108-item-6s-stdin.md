# ADR 0118: An exec takes no stdin, a caller writes its input as a guest file first, and a timeout or a signal is a result, superseding ADR 0108 item 6's stdin

> Status: accepted
> Date: 2026-11-25

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#383, in goal garamsh/naru#378. It supersedes the part of [ADR 0108](./0108-orch-serves-a-versioned-machine-api-that-names-machines-and-never-hosts-to-callers-holding-a-signer-issued-caller-certificate.md) item 6 that gives exec a `stdin` (the Exec row of its table). The rest of ADR 0108 item 6 stands: exec's command and timeout, its whole output, the running-only rule, the file calls, and that a guest call is answered by the member that took it. Its body is not edited.

## Context

- **What ADR 0108 decided.** Item 6's Exec row: `POST .../machines/{name}/exec` takes `command`, `stdin` and `timeoutSeconds` and returns the exit code, stdout and stderr. `lib/machineapi` carried `ExecRequest.Stdin`, and the conformance scenario RunningOnly ran `cat` on stdin.
- **What mvmd's Exec takes.** The runtime API `v1alpha2` (`github.com/garamsh/mvmd/lib/runtimeapi` at `v0.0.0-20261123102822-b0a6fbe45116`, the version `host/go.mod` requires) has `ExecRequest{exec_id, request_id, machine_id, command, env, working_dir}`. mvmd's `docs/architecture/runtime-api.md` at `2a07ba313c0a2c424ba9badf40ea9c36ec670e14`, in its Exec row, says the same: "the command, and optionally an environment and a working directory".
  - The output is read apart from the start (Read exec output), and a running command is reached only by Signal exec.
  - Console takes input, but the input goes to the console, not to an exec.
  - Nothing else carries input to an exec, and nothing in mvmd bounds an exec's time.
- **What the host can do.** naru's host reaches a guest only through mvmd. So it cannot pass a request's input to a command. A host-side wrapper that wrote the input to a file and ran `sh -c '... < file'` would depend on a shell in every image.
- **The PM's decision on #383 (2026-11-25).** Exec drops `stdin`; a caller writes its input as a file first; the host bounds an exec's time by Signal exec; a timeout or a signal is a result, not an error; output above 16 MiB is 413.

## Decision

1. **Exec takes no input.** In `machines/v1alpha1`, exec is `command` and `timeoutSeconds`. A body with `stdin` is 400 `invalid`, by the rule that a field the version does not name is refused. This is an alpha tightening, as the grace bound was.
2. **A caller with input writes it as a guest file first.** It calls `PUT .../files` and names the file in the command.
3. **The host bounds an exec's time.**
   - `timeoutSeconds` is at most 600. 0 or absent is 600. Above 600 is 422 `invalid`.
   - At the timeout the host ends the command by Signal exec with SIGKILL, and reads its record.
   - The request's end (the caller gone) ends the command the same way, so no exec outlives its call.
4. **A timeout or a signal is a result.** An exec that ran is answered 200 with:
   - `exitCode`;
   - `signal`, the number of the signal that ended the command, as on Linux, with `exitCode` 128 plus it, as a shell gives;
   - `timedOut`, true for a command ended at its timeout;
   - stdout and stderr.

   A command that did not start is 200 with `exitCode` 127 and the reason in stderr. A command the Machine's stop cut short is 409 `not-running`.
5. **The output is bounded.**
   - An exec's output, stdout and stderr together, is at most 16 MiB. The host reads it while the command runs, since the guest keeps only its newest output.
   - Above 16 MiB the command is ended with SIGKILL and the call is 413 `invalid`.
   - A guest file read or written is at most 16 MiB too, and a write above it reaches no guest.
6. **The bounds are the host API's and the Machine API's alike.** `lib/hostapi` and `lib/machineapi` each state 600 seconds and 16 MiB, and an orch test asserts that they are equal, as it does for the grace bound. `lib/machineapi` is a module consumed outside this repository and does not import `lib/hostapi`.

## Rationale

- **Why not wait for mvmd.** A stdin on mvmd's Exec is mvmd's change, in its API's version. garamsh/mvmd#837 proposes it. naru's exec would wait on it, and a file written first covers every input a whole exec can take.
- **How stdin returns.** With a streaming exec (an addition ADR 0108 item 6 names), on mvmd#837's input to an exec, in a Machine API version of its own.
- **Why no wrapper.** It works only in an image with a shell, and it would make one call two guest operations behind the caller's back.
- **Why a result and not an error.** The command ran, and its output up to the kill is the caller's to read. An error would drop that output, and would say nothing a field cannot.

## Consequences

- `ExecRequest.Stdin` is removed from `lib/machineapi` and its fake, and the scenarios RunningOnly and Isolation read a file a write put.
- A use that piped input into a command writes it first. garam's Workspace has no such call yet.
- The host API gains `POST /v1/microvms/{name}/exec` and `GET`, `PUT /v1/microvms/{name}/files`, all three provisioner only: a guest's file may hold its secrets, so a reader may not read one.
- orch's Machine API listener writes an answer for up to 12 minutes, the longest exec plus the host's margin, where orch's API keeps 60 seconds.
