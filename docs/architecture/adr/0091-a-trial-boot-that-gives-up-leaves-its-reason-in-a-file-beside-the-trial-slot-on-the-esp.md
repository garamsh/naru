# ADR 0091: a trial boot that gives up leaves its reason in a file beside the trial slot on the ESP

> Status: accepted
> Date: 2026-11-21

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#262 (the final check of the goal). It amends [ADR 0025](./0025-an-upgrade-is-a-bundle-posted-to-v1-upgrade-which-init-writes-to-the-trial-slot-and-the-trial-boot-promotes-when-healthy-and-rolls-back-when-not.md) items 6 to 8 by reference: no body is edited, and what that ADR says holds except where this one says otherwise.

## Context

- **ADR 0025 had a trial that gives up write its reason to the upgrade directory.** That directory, `/var/lib/naru/upgrade`, is on the data disk. The default boot that follows reads the reason from `pending.json` and records it as `lastUpgrade.reason`.
- **A trial boot is refused most often for the very reason that the directory is not there.** A trial that cannot find or mount its data disk gives up with the directory on the initramfs, which the reboot discards. The default boot found `pending.json` as the upgrade left it, with no reason, and recorded `the trial boot did not reach a healthy state` (item 7's fallback). ADR 0025's own test build, a naru-init that cannot find its data disk, is that case, and its test passed only while it asserted that the reason was not empty; #273 made init keep the refusal in the reason, asserted it, and the QEMU test failed in the final check of #262.
- **The ESP is the one disk both boots can reach whatever the data disk is.** It holds the trial slot the default boot removes, and init already mounts it for that.

## Decision

1. **A trial boot that gives up writes its reason to `\EFI\BOOT\TRIAL.WHY`**, in the directory of the slots, on the ESP that it mounts for the write as it does for a rename (ADR 0025 item 3). The file is written, `fsync`ed, and its directory `fsync`ed. `pending.json` no longer carries a reason.
2. **The default boot that settles the trial reads `TRIAL.WHY` while it removes `TRIAL.EFI`, and removes both.** The reason is that file's content; with no file, or an empty one, it is `the trial boot did not reach a healthy state`, as in ADR 0025 item 7.
3. **A reason that cannot be written is logged, and the upgrade is still rolled back** with item 7's fallback reason: the reboot that does not promote is what undoes the trial, not the reason.

## Options considered

- **An EFI variable for the reason.** It would survive the same way, but the firmware's variable store is small and shared with the boot entries, a write there is a firmware call on every board, and a stale variable would have to be found and deleted by name. A file beside the slot it explains is removed in the same step that removes the slot.
- **Keeping the reason on the data disk and making the test refuse another way.** A trial whose data disk is missing is a failure an operator meets, and its reason is the most useful one; recording it only for the failures that leave the data disk working would hide it for the case the field exists for.

## Consequences

- `lastUpgrade.reason` of a trial refused for want of its data disk begins with `the boot sequence refused: ` and says which step, as ADR 0025's tests assert.
- The ESP is mounted a second time by a trial that gives up, for the length of one write. A trial that is promoted or never gives up does not mount it for this.
- A Node upgraded across this change that has `pending.json` with a `reason` field from a trial of the older init loses it: the field is ignored, and the fallback reason is recorded.

## Signs the decision was wrong

- A board's ESP is read-only or absent at the time a trial gives up, so no reason is ever left: the answer is a second place, not this file moved.
- The reason grows beyond what a FAT directory entry's file holds cheaply; it is cut to 300 bytes for a refusal, so it does not.
