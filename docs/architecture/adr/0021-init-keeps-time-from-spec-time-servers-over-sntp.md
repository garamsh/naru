# ADR 0021: init keeps time from spec.time.servers over SNTP

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#63, part of garamsh/naru#62 and garamsh/aurora#749.

## Context

- **The clock gates the certificates.** Init issues the `spec.pki` certificates at boot and takes their validity from the Node's wall clock, which an emulated RTC gives a Node under Proxmox or QEMU ([ADR 0013](./0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md)). A Node whose RTC is wrong issues certificates that are not yet valid, or already expired, and a client that checks them refuses the Node.
- **Nothing sets the clock.** The image has no time daemon and no libc ([ADR 0004](./0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md)), so init is the only place that can.
- **A time server may be unreachable.** A Node that waits for one stops every service for a cause that is not the Node's own, so the wait must be bounded and must not refuse the boot.
- **Configuration changes live.** The class of a field decides what an apply does ([ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md)); the servers can change without a reboot.

## Decision

Init sets the clock from the SNTP servers of `spec.time.servers` (RFC 4330, UDP): once at boot, after the network and the files and before the certificates are issued, bounded to 10 s in all; then every 300 s, or 60 s after an attempt no server answered. A server is `host[:port]`, port 123 by default. Servers are asked in order and the first that answers is used. An offset of 128 ms or more either way is stepped (`settimeofday`), a smaller one slewed (`adjtimex`, `ADJ_OFFSET_SINGLESHOT`). A failure is logged and the boot goes on. `spec.time` is in the live class: an apply that adds or changes it makes init synchronise at once (`sync-time`), and one that removes it stops the polling (`stop-time-sync`). The Node API reports the state at `GET /v1/time`.

## Consequences

- A Node with `spec.time` has a clock set within the bound when a server answers, so the certificates it issues are valid by the time. A Node whose servers are all dead boots with its RTC and the certificate problem above, and logs why.
- A Node without `spec.time` boots as before and logs that its time is not synchronised.
- The worst case a dead server adds to the boot is the 10 s bound, not 3 s per server.
- The clock is not disciplined in frequency and the answers of several servers are not compared: one server's error is the Node's. SNTP's accuracy (milliseconds on a good path) is what is bought; PTP and serving NTP are out of scope.
- Polling every 300 s outlasts the 256 s a 128 ms slew takes at the kernel's 500 ppm, so a slew has ended before the next offset is measured.
- A step moves the clock backwards or forwards under running services; one that depends on a monotonic clock is unaffected, one that reads wall time sees the jump. Because the boot step comes before any service starts, only a later step, after a long drift or a changed server, can jump under them.
- The syscalls that move the clock cannot run in the tests, so the tests drive the logic with an injected clock; the QEMU test of the image covers the real ones.

## Signs the decision was wrong

- A Node's clock is wrong by more than the offsets the status reports, because one wrong server is trusted: the answer is to compare servers, which this decision rules out.
- Services misbehave at steps after boot: the answer is to slew always, or to step only at boot.
- Boots are slowed by 10 s often enough that the bound is felt: the bound is too high for the Nodes' networks.
- A deployment needs authenticated time (NTS) or sub-millisecond accuracy: SNTP over UDP is not enough.
