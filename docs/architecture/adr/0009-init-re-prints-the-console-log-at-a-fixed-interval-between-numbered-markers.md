# ADR 0009: Init re-prints the console log at a fixed interval between numbered markers

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#17, part A. It adds to what [ADR 0004](./0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md) says of init and the console; nothing in it is superseded.

## Context

- **A serial console is a stream.** A reader that attaches after a line was written does not receive it: Proxmox's `qm terminal` and its web console are readers of the guest's serial port that attach when someone opens them, and a Node is usually booted before anyone does. garamsh/naru#17 asks that such a reader still get the whole log. That the two Proxmox readers receive nothing written before they attached is the issue's statement; it was not tried on a Proxmox host for this ADR.
- **What a Node writes to its console is init's log and the services' output.** Init writes its own lines through `log/slog`; each service had the console itself as standard output and error, so init never saw those lines.
- **The log can be long and a Node can run for a long time.** The kernel's messages are not init's to keep: they are written before init runs and reach the console through the kernel, not through a descriptor init holds.

## Decision

**Init keeps every line it writes and every line its services write to standard output and error, and re-prints all of them every 30 s between a begin marker and an end marker that carry a sequence number.**

1. **The marker format is fixed.** A block is `NARU-LOG-BEGIN seq=<n> lines=<k> dropped=<d>`, then the `k` kept lines, then `NARU-LOG-END seq=<n>`, each marker on a line of its own. `seq` is 1 for the first block and rises by one. `dropped` is the number of lines dropped since boot (item 3). A block is written in one write, so no other line is inside it, and when the console is in the middle of a line when the block starts, the block starts on a new line.
2. **Services write to a pipe init reads.** Init gives each service a pipe as standard output and error and copies what it reads to the console and to the kept lines. A service's lines are not changed.
3. **The memory is bounded at 1 MiB of kept lines.** A line longer than 4096 bytes is kept as several lines of at most 4096. When the kept lines pass 1 MiB, the oldest are dropped and counted; the begin marker's `dropped` says how many, so a reader knows the block starts after the log's beginning. The worst case is one re-print of 1 MiB per 30 s.
4. **Kernel messages are not kept.** Only what passes through init is.
5. **A reader takes a block as a repetition.** A line may appear many times, once as written and once in each block after it. A reader that asks whether a line is in the log strips the markers and asks that; `make judge-serial` does.

## Consequences

- A reader that attaches at any time has the whole log, up to the bound, within 30 s.
- A reader that has watched from the start sees every line again at each interval, so a log read as a stream is longer than the log, by up to 1 MiB per 30 s.
- A service's standard output is a pipe, not the console, so it is not a terminal; a service that tests for one sees none. The services of P0 do not.
- A service that writes faster than the console accepts is held by the pipe, as it was held by the console before.
- The reader of a log must know the markers, so the format is part of the interface and changing it is a new ADR.

## Signs the decision was wrong

- A serial reader or the host drops or stalls on the repeated volume, or a Node's output is lost while a block is written.
- A service is found that needs a terminal as standard output.
- Logs routinely pass 1 MiB before the fault is read, so `dropped` is not zero where the cause was.
- Proxmox's readers turn out to keep a scrollback, so the re-print repeats what they already have.
