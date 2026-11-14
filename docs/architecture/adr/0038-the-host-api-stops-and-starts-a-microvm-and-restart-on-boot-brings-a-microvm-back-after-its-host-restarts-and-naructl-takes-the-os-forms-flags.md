# ADR 0038: the host API stops and starts a microVM, restart-on-boot brings a microVM back after its host restarts, and naructl takes the OS form's flags

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#110 (garamsh/aurora#804 Amendment 1, naru criterion 3). It extends [ADR 0028](./0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md) and [ADR 0035](./0035-the-host-api-creates-a-microvm-of-an-os-form-from-a-boot-set-with-a-data-disk-and-a-nocloud-seed-and-the-iso-9660-writer-is-a-library-module.md) by reference: their bodies are not edited, and what they say holds with this decision added.

## Context

- **A control plane above the Node must stop a microVM without deleting it,** and start it again, and must find its microVMs running after the host restarts.
- **mvmd has `restart_on_boot`** (`CreateMachineRequest` field 19, `Machine` field 20), fixed for the Machine's life. mvmd starts a Machine that was Running, or being started, when the host went down and had no Stop, Pause or Delete since, as an ordinary Start at the first fencing arming of the new boot. A Machine whose VMM ended in the same boot is not restarted. A failed restart leaves the Machine Stopped with a `last_failure_reason` that begins "the restart after the host's boot changed failed"; mvmd does not retry.
- **ADR 0035 left naructl's flags for the OS form open.**

## Decision

1. **`POST /v1/microvms/{name}/stop` and `POST /v1/microvms/{name}/start`,** each for the `naru:host-provisioner` role only (a reader is 403). Each answers 200 with the microVM as it is after. A stop of a microVM that is not running, and a start of one that is running, answer 200 without a call to mvmd, so a repeat is safe. A name mvmd does not hold is 404. A start that mvmd refuses in the Machine's state is 409.
2. **`restartOnBoot` is a field of `POST /v1/microvms`,** true for the OS form and false for the image form when the request does not say. hostd passes it to mvmd's `restart_on_boot`, shows it in the microVM, and compares it when a repeated request names an existing microVM: a different value is 409, as mvmd fixes it for the Machine's life.
3. **hostd does not retry a failed restart.** The reason mvmd records is the microVM's `failure` in `GET /v1/microvms/{name}`; the caller starts the microVM again or deletes it.
4. **A microVM stopped through the host API stays stopped across a host restart.** This is mvmd's rule for a Machine with a Stop since the boot, and hostd adds nothing to it.
5. **naructl** gains `microvm stop <name>` and `microvm start <name>`, and for `microvm create` the OS form's flags `--boot-set`, `--cmdline`, `--seed FILE` (the user-data), `--data-disk` and `--pod-prefix`. The flags of the two forms are not mixed. `-f` accepts the OS form's fields and `restartOnBoot`.
6. **The host restart is shown on the QEMU harness:** a microVM created with `restartOnBoot` is `running` again after the Node reboots through the Node API, one stopped before the reboot is not, and the time to return is logged.
7. **The mvmd pin moves to 191326aca748a0e6e365578016ab87455107164e** (mvmd#730, which carries `restart_on_boot`), as the PM decided: hostd's runtimeapi, `image/initramfs/pin.env` and `image/guest/pin.env`.

## Consequences

- A caller reads a microVM's `state` and `failure` to learn whether a host restart brought it back, and has no other signal.
- A microVM of the image form that should return after a host restart is created with `restartOnBoot: true`.
- `tooling/qemu/go.mod` and `tooling/probe/go.mod` keep the earlier runtimeapi pseudo-version until a change in their own paths moves them.

## Signs the decision was wrong

- A failed restart is common: hostd gets a retry, or a field that says how many times.
- A caller needs a stop that a host restart undoes: `restartOnBoot` is not the whole of it.
- mvmd changes `restart_on_boot` to settable after creation: the 409 on a different value goes.

## Errata

- 2026-11-15: paths moved in the image split ([ADR 0054](./0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)): `image/guest/pin.env` is `image/host/guest/pin.env` and `image/initramfs/pin.env` is `image/host/mvmd/pin.env`.
