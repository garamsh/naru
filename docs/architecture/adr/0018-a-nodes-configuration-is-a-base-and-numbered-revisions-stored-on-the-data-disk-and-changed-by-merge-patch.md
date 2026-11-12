# ADR 0018: a Node's configuration is a base and numbered revisions stored on the data disk, and is changed by merge patch

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#46, part of goal garamsh/naru#44. The direction is decision D5 of the PM's design comment, https://github.com/garamsh/naru/issues/44#issuecomment-6824452883. Details the comment left open, and the patch format, were decided in the PM's review of garamsh/naru#50 and are recorded as written. Talos is cited by concept only (a versioned machine configuration, patches that merge named lists by key as its strategic merge does, apply modes). It supersedes nothing: [ADR 0008](./0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md) stays how configuration reaches a Node, and ADRs 0012 to 0016 stand. It rides on the Node API of [ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md).

## Context

- **Configuration is read once, at boot.** Init parses `user-data` before the network and acts on it in a fixed order ([init.md](../init.md) §Current decisions, Boot order); changing anything means a new NoCloud drive and a reboot.
- **Init already restarts services on a change of its own making.** `spec.pki` certificates name `restart` services and init restarts them after a renewal ([ADR 0013](./0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md) item 7); the service manager has `Start`, `Stop` and `Restart` per service.
- **The data disk is the Node's persistent store** at `/var/lib/naru` ([ADR 0012](./0012-gaon-runs-on-the-nodes-with-etcd-on-the-data-disk-carried-certificates-and-a-host-forward-to-the-api-server.md) item 4), mounted at boot step 5, after the network.
- **Operators replace user-data, too.** Proxmox snippets replace it as a file, and that replacement has to win over anything an earlier apply stored.
- **The data disk is not the base's.** A data disk can outlive a user-data, or be replaced under one.

## Decision

**The NoCloud `user-data` is the base, at revision 0. Each successful apply stores a new revision on the data disk. At boot init uses the stored configuration only when it was derived from the user-data it now finds.**

1. **Revisions.** The base is revision 0. Each successful apply produces revision N+1, where N is the revision the request named.
2. **Store.** `/var/lib/naru/config/` on the data disk holds the effective configuration, its revision and the SHA-256 of the `user-data` bytes the base was. A write is atomic: a temporary file, `fsync`, rename. Revision, digest and configuration are one file, so they cannot disagree: `store.yaml`, a YAML document with `revision`, `baseSha256` and `config`.
3. **Request.** `POST /v1/config` takes `{"revision": number, "patch": object | "config": object, "mode": "no-reboot" | "staged", "dryRun": bool}`: exactly one of `patch` and `config`; `mode` is required, with no default; `dryRun` is false when absent.
   - `patch` is a **merge patch with keyed lists**, applied to the stored configuration as JSON: a JSON merge patch (RFC 7386) with one extension, in the way of the strategic merge of Talos's configuration patches. `config` replaces the configuration whole.
     - A list whose elements carry a key merges by that key: `spec.services` by `name`, `spec.files` by `path`, `spec.pki.authorities` by `name`, `spec.pki.certificates` by `name`.
     - A patch element whose key matches an existing element is merge-patched into it. An absent key leaves the element unchanged. An element with a new key is appended, and existing elements keep their order, which is the start order of `spec.services`.
     - An element with `"$patch": "delete"` and its key removes that element. A delete of a key that does not exist is refused with 422.
     - Every other list (`args`, `dnsNames`, `restart` and the rest) is replaced whole, as RFC 7386 does.
   - A `revision` that is not the stored one is refused with 409, as stale.
   - The result is parsed and validated by the parser and validator init uses at boot. A result that fails is refused with 422, naming the field.
   - A request is applied one at a time; a second one sees the first's revision and is stale.
   - Every refusal leaves the Node unchanged: nothing stored, nothing live.
4. **Response.** On success: `{"revision": number, "mode": string, "dryRun": bool, "actions": [{"action": string, "target": string}]}`, with `revision` the one stored after the call (unchanged for `dryRun`). The actions are `store`, then per field below `start`, `stop`, `restart`, `write-file`, `issue-certificate` and `reload-api`, with the service, path, certificate name or `spec.api` as target. `dryRun` lists what a real call would do and does none of it, the store write included.
5. **Modes.**
   - `no-reboot` applies live. It refuses with 422, listing the fields, a change to a field that needs a reboot, and a change to a field no call may change.
   - The classes below apply to the result of the merge: a field is changed when the result differs from the running configuration in it, so a deleted or added keyed element counts as a change of its list.
   - `staged` stores the result for the next boot and takes no live action. It accepts a reboot-only field.
   - A `reboot` mode waits for the reboot follow-up ([target.md](../target.md)).
6. **The stored configuration is what `GET /v1/config` returns and what the next patch applies to.** `no-reboot` computes its live actions against the configuration running now, and refuses when the result differs from the running one in a reboot-only field. So a `staged` change to such a field makes every later `no-reboot` apply refuse until a reboot makes it the running one.
7. **Fields.**

   | Class | Fields | What applying does |
   |---|---|---|
   | Live | `spec.services` | per service name: start the added, stop the removed, restart the changed; an unchanged service is not touched and keeps its PID |
   | Live | `spec.files` | rewrite each changed file; the services in the file's new `restart` list are restarted |
   | Live | `spec.pki.certificates` | issue each changed certificate again, and restart the services its `restart` names |
   | Live | `spec.api` | the listener reloads its pair and `clientCA` |
   | Reboot-only | `spec.controlPlane`, `spec.network`, `spec.pki.authorities` | refused by `no-reboot`, stored by `staged` |
   | Never | `metadata.name`, `spec.dataDisk` | refused in every mode, `staged` included: the first is the Node's identity, the second holds the store |

   `apiVersion` and `kind` are never changed either. Within `spec.api`, `listen` and the presence of `spec.api` itself are reboot-only: a call cannot rebind the listener that answers it, and an API that is off cannot be turned on by a call to it.
8. **`spec.files[].restart`** is a new optional list of service names in `spec.services`, validated as `spec.pki` certificates' `restart` is. A change to the file's `content`, `mode` or `path` restarts those services, once per apply however many files name one. A configuration without it parses as before.
9. **Order and failure.** The store is written before any live action. A refusal before the store changes nothing. A live action that fails after it is reported as 500 with the actions taken and the one that failed; the stored revision N+1 stays and init does not roll back, so the next boot reaches the stored configuration.
10. **At boot.** Steps 1 to 5 of init's boot (rootfs, mounts, NoCloud, network, data disk) use the base. After the data disk mounts, init chooses the effective configuration: the stored one when its `baseSha256` equals the SHA-256 of the current `user-data` and it validates; otherwise the base, logging that the stored configuration was discarded and why (the base changed, or the stored configuration does not validate), and storing the base as revision 0. A data disk with no store gives the base. An operator who replaces the user-data therefore always wins. Only `user-data` is digested: a changed `network-config` alone keeps the store.
11. **Compatibility with v0.3.0.** A v0.3.0-form `naru.yaml` parses unchanged: no field is required that v0.3.0 did not require, `spec.api` and `restart` are optional, and a Node without `spec.api` has no way to apply, so it runs its base at revision 0, as v0.3.0 does. A v0.3.0 data disk has no `/var/lib/naru/config/`, which is the "no store" case.

## Consequences

- **A running Node can change a service's arguments, a file or a certificate without a reboot**, and only the services whose entries changed restart.
- **A patch to one service or file names only that element.** Two operators' patches to different services do not collide, and a patch that changes one service's `args` carries that service's `name` and the new `args` (a list inside an element is replaced whole). Patch handling is a merge with one extension to RFC 7386, which init implements itself; the keys are fixed to the four lists above, and a new keyed list is a new decision.
- **The store holds secrets.** The stored configuration is the user-data's contents, which include `spec.pki` authority keys and `spec.files` content, on the data disk. `GET /v1/config` redacts them ([ADR 0017](./0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) item 7); the file does not. The data disk was not holding them before.
- **A data disk that is replaced loses applied changes**, and the Node runs its base again at revision 0. A base replaced under a live Node discards them too, deliberately.
- **A revision history is not kept.** Only the effective configuration is stored, so there is no revert to revision N-1; a revert is a patch.
- **The apply engine and boot share the validator**, so a configuration the API accepted is one init accepts at boot.
- **The boot order changes**: step 5's data disk now precedes the choice of configuration, and steps 6 to 11 act on the effective one. [init.md](../init.md) §Decided, not yet implemented says so.

## Signs the decision was wrong

- A list that needs a per-element merge is not one of the four keyed lists, or an operator needs to edit one element of `args`, which would say the patch format should key more lists or merge by position.
- An apply that fails after the store leaves a Node whose next boot differs from what runs, and operators need it undone, which would say the store should be written after the live actions or keep a previous revision.
- The base digest discards a store an operator wanted to keep, for example when user-data is regenerated byte-different with the same meaning, which would say the digest should be of the parsed configuration.
- `staged` followed by `no-reboot` refusing (item 6) surprises operators often, which would say a Node should report its running revision and `no-reboot` should compare against that and the stored one.
- Secrets on the data disk are found by a reader of the disk who could not read them in the user-data, which would say the store should hold the changes and not the whole configuration.
