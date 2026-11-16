# ADR 0081: A bootstrap token is added or revoked by a live patch to the control-plane Node's token list file, from the operator's token store

> Status: accepted
> Date: 2026-11-16

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#54, part of goal garamsh/naru#235. It supersedes [config.md](../config.md)'s procedure to add a Node ("apply that new user-data … the control-plane Node restarts with it") and the consequence "Adding a Node needs the control-plane Node's user-data again" of [ADR 0016](./0016-a-nodes-agent-gets-its-pair-from-a-signing-request-with-a-bootstrap-token-and-the-release-publishes-a-test-ca-key.md). It builds on [ADR 0018](./0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) (the patch, `files[].restart`) and sketch 6 of [target.md](../target.md).

## Context

- **Adding a Node recreated the control-plane Node's user-data and rebooted it.** The token list is a file written from `spec.files` and the API server reads it at start, so a new token meant new user-data for the control-plane Node and a reboot that took the control plane down for every Node.
- **The Node API already changes a file and restarts a service.** ADR 0018 item 8 gives `spec.files[].restart`, and a merge patch names a `spec.files` entry by `path`, so a live change to one file needs no new endpoint.
- **A read of the list is redacted.** `GET /v1/config` replaces the content of a file that group or other cannot read, and the list is `0600`, so a patch cannot be built from a read of the running Node.
- **Kubernetes Nodes take tokens too.** goal #235 reads the issue as the Kubernetes Node's bootstrap tokens, added and revoked live on the control-plane Node as above; gaon's already exist, and a Kubernetes Node has had a pair issued by `naructl` and none of its own.

## Decision

**`naructl token create` and `token revoke` change the operator's token store and patch the control-plane Node's list file through the Node API, with no reboot.**

1. **`gen node` gives the list file `restart: [gaon-apiserver]`.** The entry `/run/gaon/bootstrap-tokens` carries it, so an apply that changes its content restarts the API server and nothing else. A disabled service is dropped from the list as it is from `spec.pki`'s. The Node's own token file has no restart.
2. **`naructl token create --ca-dir <dir> --name <name> --config <client file>`** makes the Node's token in `<dir>/tokens/<name>` as `gen token` does (it keeps one that is there), then patches `spec.files` of the Node the client file reaches with one entry: `path`, `mode` `0600`, `restart` and `content`, the whole list, one token per line.
3. **`naructl token revoke`** deletes `<dir>/tokens/<name>`, refuses with exit 1 and no call when there is none, then patches the list as in 2.
4. **The content is the store's, not the Node's.** The patch always carries every token of `<dir>/tokens`, in the order of the names, because the Node's list cannot be read (Context). A token present on the Node and absent from the store is dropped by the next create or revoke; one operator's store is the record.
5. **Both patches use the `no-reboot` mode.** Init writes the file and restarts the API server; a Node Agent that is registered keeps its pair, since only a signing request needs a token.
6. **`gen token` and `gen node` stay.** A Node's user-data is written with its own token; `token create` is what puts the token on the control-plane Node. A Node whose token is added live is started after the call and needs no change to the control-plane Node's user-data.
7. **The Kubernetes form is the same mechanism with `--kubernetes`.** `gen kubernetes-node --control-plane --bootstrap-tokens` makes the API server take `--token-auth-file /run/kubernetes/bootstrap-tokens.csv` (a `0600` file, one line per token, `system:bootstrap:<id>` in `system:bootstrappers`, `restart: [kube-apiserver]`), the controller manager sign with the authority's key (`keyPath` `/run/kubernetes/pki/ca.key`), and `gen kubernetes-node --role worker --token` has the kubelet present the token (`--bootstrap-kubeconfig`, its certificates in `/var/lib/naru/kubelet/pki`) in place of an issued kubelet-client pair. `token create --kubernetes` and `token revoke --kubernetes` patch the CSV. The cluster needs the ClusterRoleBindings of `layout.BootstrapRBAC` applied once by the administrator, which naru does not apply (it has no Kubernetes API client).

## Consequences

- **A token is added or revoked without the control-plane Node's user-data or a reboot.** The API server restarts, which takes its API down for the seconds it needs; agents retry.
- **Revoking stops joins, not Nodes.** A Node that already holds a certificate keeps it until it expires; the token only decides who may ask for one.
- **The store is the single record**, so an operator with a second store loses the tokens only the first holds on the next patch (item 4).
- **The control-plane Node's user-data still lists the tokens present when it is written.** It is the base; a patch is a revision on top, so a reset of the revisions returns the list to the base's.
- **The Kubernetes form is unverified under QEMU.** Its layout is tested as user-data and as a patch; no test boots a Kubernetes Node that joins by a token, and whether the API server accepts an empty static token file is not established. The gaon form has a QEMU test that adds a token live, joins a third Node, revokes a fourth's and checks the control-plane Node's boot ID.

## Signs the decision was wrong

- Operators run several stores and lose tokens to item 4, which would say the Node API needs a read of the list, or a dedicated token endpoint (out of scope here).
- A restart of the API server for a token interrupts something that cannot retry, which would say a token needs a path that does not restart.
- A Kubernetes join by token needs a change to the API server's arguments, which a live patch to a file cannot make.
