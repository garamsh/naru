# ADR 0036: a re-print carries at most the newest 16 KiB of the console log, and naru-vip's liveness probe is a TLS request the API server can answer

> Status: proposed
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#105. It amends [ADR 0009](./0009-init-re-prints-the-console-log-at-a-fixed-interval-between-numbered-markers.md) (what a block carries) and [ADR 0024](./0024-the-control-plane-is-three-etcd-members-with-tls-on-both-ports-and-a-virtual-address-held-by-naru-vip-behind-a-fence-on-etcds-ports.md) (what the probe sends) by reference: their bodies are not edited. [ADR 0030](./0030-no-writer-of-the-console-waits-on-the-console-device-one-goroutine-writes-it-from-a-bounded-queue-and-a-loss-is-counted-on-the-device.md) stopped writers waiting on the device; it did not stop the device costing the guest time, which is what this finds.

## Context

- **Aurora's Nodes still stalled after ADR 0030** (`v0.1.0-dev.7`): pings to two control-plane Nodes took 4 to 24 s, and `naru-vip` released the address for 18 to 20 s at a time. A `socat` reader draining each serial socket at about 37 kB/s did not ease it (garamsh/naru#105, comment 6867231861), so "QEMU stops draining while nobody reads" is not the cause.
- **37 kB/s is the re-print.** `KeptBytes` is 1 MiB and the interval is 30 s; 1 MiB / 30 s is 35 kB/s. Once a Node's kept log has filled, every re-print writes 1 MiB to the serial port.
- **In the QEMU harness the stall sits on the re-print's 30 s grid, with or without a reader.** Three control-plane Nodes, serial port as given, a TLS handshake to each Node 5 times a second for 120 s starting 90 s after the vip service ran, init as of `v0.1.0-dev.7` (before this change). By the end the console had held 7 re-prints of about 1000 lines (about 100 KB) per Node:

  | serial port | handshakes over 100 ms (node-1 / node-2 / node-3) | slowest |
  | --- | --- | --- |
  | unix socket, no client | 0 / 3 / 3, at 25 s, 55 s and 115 s of the window | 0.54 s |
  | unix socket, a `socat` reader | 1 / 2 / 3, on the 30 s grid | 0.91 s |
  | file | 13 / 17 / 5, on the 30 s grid | 1.48 s |
  | unix socket, a client that connected and never reads | 0 / 0 / 0 | 0.04 s |

  The stalls are as long as the block is large: 0.3 to 1.5 s for about 100 KB, so 3 to 15 ms per KB, which for 1 MiB is 3 to 15 s, the order of the 4 to 24 s aurora measured. A harness Node's kept log is a tenth of 1 MiB, so the harness shows the cost at a tenth of aurora's. These are single runs on one host, and the row with the connected client shows that how QEMU treats the socket moves the numbers. The harness did not reproduce a release of the address: its stalls are under the 3 missed seconds `naru-vip` waits for.
- **What this does not separate.** Whether the time goes to the kernel's 8250 console path or to init's writes through the tty was not isolated: the kernel console was not moved off ttyS0, which needs a kernel rebuild, and no run was made with a console that carries only init's own lines. The 8250 busy-wait hypothesis is neither confirmed nor refuted. What the numbers show is that the stall grows with the bytes a block writes.
- **The probe made the API server log a line per probe.** `naru-vip` probed by dialling the API server's port and closing, so each probe was a failed TLS handshake, and the API server logged `http: TLS handshake error from <member>: EOF` for it: three probers, one line a second each, kept and re-printed with the rest.

## Decision

1. **A re-print carries the newest kept lines that fit in 16 KiB** (`console.ReprintBytes`), not every kept line. The begin marker's `dropped` is the number of lines since boot the block does not carry: dropped from the kept lines (ADR 0009) or older than the 16 KiB. `lines` is the number it carries. The kept lines, their 1 MiB bound, `Lines` and so `naructl logs` are unchanged: `naructl logs` stays complete.
2. **`naru-vip`'s probe of a member's API server is an HTTPS `GET /`** over TLS 1.2 or later, verifying the serving certificate against the CA given by `-etcd-ca-file` with the server name set to the virtual address, which the certificate always names. Any HTTP response counts as alive, so the probe needs no client certificate and does not depend on what the API server authorizes. A refused dial, a failed handshake, a certificate from another CA or for another name, and no response in the timeout are not alive. The etcd health check of ADR 0024 is unchanged. The probe finishes the handshake, so it leaves nothing in the API server's log.
3. **The harness can give a Node's serial port as a unix socket nobody reads** (`Spec.SerialSocket`, `ClusterSpec.SerialSocket`), the shape of a Proxmox `serial0: socket`.
4. **A QEMU test holds both** (`TestControlPlane_ThreeNodesWhoseSerialSocketNobodyReadsAnswerWithinAHundredMillisecondsAndTheHolderNeverReleasesTheAddress`): three control-plane Nodes with unread serial sockets, probed 5 times a second each for 120 s starting 60 s after the vip service runs. No handshake is slower than 100 ms, node-1's vip log has no new `released` line, and no service's log has `TLS handshake error`.

## Consequences

- A re-print is at most 16 KiB plus markers: 0.5 kB/s on the serial port instead of 35 kB/s.
- **A reader that attaches late no longer has the whole log within 30 s.** It has the newest 16 KiB, and the begin marker says how many lines it lacks; the rest is in `naructl logs` while the Node API answers. This replaces ADR 0009's "a reader that attaches late has the whole log within 30 s". The judge and the QEMU tests read a console from boot, and are unaffected.
- A reader of the log treats a line as possibly repeated, as before, and cannot assume the first block it sees starts at boot.
- A control-plane Node's API server answers a request per member per second where it logged a failed handshake.

## Signs the decision was wrong

- A Node stalls on the 30 s grid with a 16 KiB block: the cost is not in the bytes of the block but in the write, and the block should be written in paced pieces.
- A Node still stalls with the console carrying only init's own lines: the serial port costs the guest per write whatever the volume, and the kernel and init should not write ttyS0 once a Node is up.
- An operator needs the first lines of boot from the serial console of a Node whose Node API is down: 16 KiB is too small, or the first block should be kept in full.
- An API server that answers HTTP at every path but cannot serve (a 5xx) keeps the address with its holder: the probe should read the status.
