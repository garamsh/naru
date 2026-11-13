# ADR 0030: no writer of the console waits on the console device, one goroutine writes it from a bounded queue, and a loss is counted on the device

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#85, on the critical path of garamsh/aurora#749. It amends [ADR 0009](./0009-init-re-prints-the-console-log-at-a-fixed-interval-between-numbered-markers.md) by reference: its body is not edited, and the marker format, the kept lines and their bound hold with this decision added. It replaces two things 0009 says: that a service is "held by the pipe" when it writes faster than the console accepts, and that a block is written while the console is held. Together they are what the issue found.

## Context

- **A re-print held the console's mutex while it wrote the block.** The block is up to 1 MiB and the serial device of a Proxmox guest is a socket nobody may read, so the write can take seconds or not return. Every service's next line, init's own log, and `GET /v1/services/{name}/logs` took the same mutex, so each waited with it.
- **A service that waits on its own logging stops serving.** On aurora's Nodes at `v0.1.0-dev.6`, `naru-vip` released the virtual address on the 30 s grid of the re-print, 14 times in the readings of garamsh/naru#85, with etcd's slow reads and one Node API timeout in the same seconds. The cause is read from the code, not reproduced on a Proxmox device. The QEMU tests read the serial device at once, which is why they never showed it.
- **ADR 0009 named this as a sign the decision was wrong:** "a Node's output is lost while a block is written", and "a serial reader or the host drops or stalls on the repeated volume".

## Decision

**No writer of the console waits on the console device. Output reaches the device through one goroutine fed by a bounded queue, and what the device is not given is counted and said on the device.**

1. **The mutex guards memory only.** It guards the kept lines and the queue, and is never held while the device is written. This covers a service's pipe, init's own log, a re-print, `Lines` and so `naructl logs`.
2. **One goroutine writes the device**, in the order the writes were queued, started when something is queued and ended when the queue is empty.
3. **Live output is bounded at 256 KiB queued** (`console.QueueBytes`). A write that would pass it is not written to the device, is counted in bytes, and is still kept: `naructl logs` and the next block hold it, up to the kept bound of ADR 0009.
4. **A re-print copies the kept lines under the mutex and queues the block** as one write, then returns. At most one block is queued or being written. A re-print due while one is, queues nothing, is counted as skipped, and does not use a sequence number, so `seq` stays contiguous in what the device received. A block is not counted in the 256 KiB; the memory bound is 256 KiB of live output plus one block of at most 1 MiB.
5. **A loss is reported by a line of its own:** `NARU-CONSOLE dropped=<bytes> skipped=<re-prints>`, the bytes of live output not queued and the re-prints skipped since the last such line. The writer writes it as the next write after the one in flight returns, which is when the device has room, starts it on a new line when the console is mid-line, and writes it only when one of the counts is not zero. It is never inside a block.
6. **The block format of ADR 0009 is unchanged.** A reader that strips the markers sees the report as one more line. `tooling/qemu`'s judge reads only `NARU-LOG-BEGIN` and `NARU-LOG-END` as markers.
7. **Init waits at most 2 s for the queue before it reboots or powers off**, so the last lines reach a device that takes them, and goes on when it does not.

## Consequences

- A service, init's own log and the Node API no longer wait on the console device. A device that never takes a write costs the device its lines and nothing else.
- The device can lose live lines and blocks. The kept log does not lose them, so a reader that reaches the Node API has everything up to the kept bound.
- A reader of the serial console can tell from the report that a gap was a loss and how large.
- A line queued is written later than it was logged when the device is slow, so the console's order is the order of queueing, not a timestamp.

## Signs the decision was wrong

- `NARU-CONSOLE` lines appear on a Node whose device is read, so 256 KiB is too small for a burst or a block takes longer than 30 s.
- On three idle members the holder logs a release again with no `NARU-CONSOLE` line: the console was not the cause, and this ADR fixed something else.
