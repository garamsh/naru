# ADR 0022: a Node is rebooted through the Node API by POST /v1/reboot or the reboot apply mode, and init answers before it stops

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#53. It amends [ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) (the endpoints) and [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) (the modes) by reference: their bodies are not edited, and what each says about the endpoints and the modes holds with this decision added.

## Context

- **A reboot-only field has no way to take effect through the API.** `staged` stores such a change and waits for a reboot that an operator must cause some other way, so a change of `spec.network` takes two tools ([ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md)).
- **An in-place upgrade ends in a reboot.** The follow-up that upgrades a Node in place (garamsh/naru#67) must be able to reboot the Node it just wrote to, through the same path an operator uses.
- **The caller's connection ends with the reboot.** A reply written after the services stop is never delivered, and a reply that waits for the reboot is a request that never completes. The client must have its answer first.
- **Init already stops and reboots on SIGINT.** The path stops the services, closes the API server, syncs, unmounts the data disk and reboots ([init.md](../init.md)). A second path would be a second place where the order of those steps can drift.

## Decision

1. **`POST /v1/reboot`** needs `naru:admin`, like every request that is not a `GET`, takes no body, and answers `202 Accepted` with `{"actions":[{"action":"reboot","target":"node"}]}`. It is served whenever the API is, including on a Node whose boot was refused, since that is a Node an operator most needs to reboot.
2. **The `reboot` apply mode.** `POST /v1/config` accepts `mode: "reboot"`. It validates and stores as `staged` does, so a reboot-only field is accepted, takes no live action, lists `store config` and `reboot node` among its actions, and, once the store has succeeded, reboots. A refused change (409, 422), a failed store (500) and a `dryRun` reboot nothing; a dry run lists the `reboot node` action it would take. The Node boots on the stored revision by the boot choice of ADR 0018. `naructl reboot` and `naructl apply --mode reboot` are the client forms.
3. **The reply goes first.** For either request the handler sets `Connection: close`, writes the reply and flushes it to the connection, and only then calls init's reboot, which returns at once. The reply of `POST /v1/config` in `reboot` mode is `200` as in the other modes; that of `POST /v1/reboot` is `202`, because it reports an action begun, not a change made.
4. **One stop path.** Init's reboot is a SIGINT delivered to its own signal channel, so the services stop, the API server closes, the disk is synced and unmounted and the Node reboots exactly as Ctrl-Alt-Del does. Nothing else stops the services for the API.
5. **No change after a reboot is accepted.** Once a reboot is accepted the API answers `503 {"error":"the Node is rebooting"}` to every request that is not a `GET`, so no apply starts a service the stop has just ended. Reads are still answered until the server closes.

## Consequences

- A change of `spec.network` or another reboot-only field is one command, and the Node is back on the new revision. [ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)'s roles need no change: a reader cannot reboot a Node.
- The in-place upgrade of #67 calls `POST /v1/reboot`, or applies in `reboot` mode, and decides nothing that this closes: the endpoint takes no body, and the mode composes with any stored change.
- The client is told the Node accepted the reboot, not that it came back. A caller that needs the new boot reads `bootId` from `GET /v1/version` until it differs ([ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)).
- The gate of item 5 is not a lock. A request past the check when the reboot is accepted still runs, and an apply already in the engine finishes under the engine's lock while the services stop. The stop is what ends it, and the next boot starts from what was stored, because the store is written before any live action ([ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md)).
- The reply is flushed, not acknowledged. A client whose connection fails after the flush sees a transport error, not a refusal, and cannot tell from the error whether the Node is rebooting; it reads `bootId` as above.
- Rebooting is as destructive to the services as SIGINT is: every service is stopped, and a Node whose boot is refused again stays down.

## Signs the decision was wrong

- A client regularly loses the reply to a reboot it caused, because a proxy or a TLS stack drops a closing connection before it reads: the answer is a reply the client confirms, which this decision does not have.
- The window of item 5 shows in practice, as a service started after the stop: the answer is a gate inside the engine, not at the door.
- #67 or another caller needs to reboot into something other than the stored revision, or to cancel an accepted reboot: the endpoint then needs a body.
