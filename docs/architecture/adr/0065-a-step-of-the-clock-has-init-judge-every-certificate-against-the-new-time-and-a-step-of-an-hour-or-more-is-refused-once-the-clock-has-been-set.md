# ADR 0065: a step of the clock has init judge every certificate against the new time, and a step of an hour or more is refused once the clock has been set

> Status: accepted
> Date: 2026-11-15

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#193, part of garamsh/naru#197. It amends [ADR 0013](./0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md) (when a certificate is renewed) and [ADR 0021](./0021-init-keeps-time-from-spec-time-servers-over-sntp.md) (what a step is allowed to be), and does not edit either.

## Context

- **The boot goes on when no time server answers.** The certificates are then issued on the RTC's clock. When a server later answers, the syncer steps the clock, possibly by years.
- **The issuer was not told.** Its renewal time was computed once, at two thirds of the validity, and kept as a monotonic reading, which a step of the wall clock does not move. After a forward step of years the Node served certificates that were already expired in real time, for up to about 20 days of a 720 h validity.
- **A step of any size was applied.** A server that is wrong by years, asked after the clock was right, moved every certificate's validity with it.

## Decision

**The syncer reports each step, the issuer re-evaluates every certificate against the new time, and a step of one hour or more is refused once a synchronisation has succeeded since boot.**

1. **`timesync.Syncer.OnStep(fn)`** has `fn` called with the result after each synchronisation that stepped the clock. `lib/init/linux`'s `Host.OnTimeStep` gives it to the boot sequence, and `bootAndRenew` registers the issuer's `ClockStepped` through it before the boot starts.
2. **`pki.Issuer.ClockStepped`** has `Run` re-evaluate every issued certificate at once. One that is not valid at the new time (its `NotBefore` is after it, or its `NotAfter` before it) is renewed like a due one: a new key, the pair placed, the services it names restarted once. One still valid keeps its pair, and its renewal time is worked out again from the new time. A renewal that fails is retried after a minute, like any other.
3. **The renewal time is wall-clock time** (no monotonic reading), so a step moves it with the certificate's validity.
4. **The bound is `timesync.StepBound`, one hour, and its rule is: accept at the first synchronisation since boot, refuse later.** The first successful synchronisation steps the clock by any offset, because a Node's RTC may be years wrong. After one has succeeded, a step of `StepBound` or more either way is refused: the clock is left as it was, the server is counted as having failed (the next is asked), and when none steps, the attempt fails with `timesync.ErrStepTooLarge`. A refusal is visible where a failed attempt already is: `lastError` and `lastAttemptAt` of `GET /v1/time`, and the `time not synchronised; will try again` warning at every retry (each 60 s).
5. **The operator's cure for a refusal is a reboot.** A Node whose clock is really more than an hour off after it had been set (a guest paused for a day, say) retries for ever and refuses each time until it reboots, when the first synchronisation steps it.

## Consequences

- A certificate issued on a wrong clock is renewed within a pass of the clock being stepped, and its services restart.
- A backward step also renews: a certificate whose `NotBefore` is in the future of the new clock is not valid for a peer.
- The authority must be valid at the new time for a renewal to succeed; if it is not, the renewal is logged as failed and retried.
- A lying or broken time server cannot move a set clock by an hour or more. It can still move it by less, which ADR 0021 already accepted.
- The first synchronisation is unbounded, so a lying server at the first synchronisation sets the clock. ADR 0021 accepted that too (one server is trusted).

## Signs the decision was wrong

- Nodes that are paused or resumed often hit the refusal: the bound is too low or the rule should refuse only when the certificates would be affected.
- A deployment needs a step the first synchronisation did not take, for instance after `spec.time` is pointed at a new server: the rule should reset on a change of servers.
