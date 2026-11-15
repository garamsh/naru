# lib/layout

The gaon Node layout: the one writer of a Node's user-data, in pure Go over `go.yaml.in/yaml/v3`.

- `layout`: a Node's services and their arguments, `spec.pki`, `spec.host`, the worker's files, mvmd's configuration, the data disk's paths and the Kubernetes Node's layout, and the topology the harness reads from `naructl topology`.
- `layout/pki`: the cluster CA, the certificates a Node is given and the bootstrap tokens the layout writes.

Linked by `naructl`, and by orch to build a joining host's configuration (`docs/architecture/structure.md`, ADR 0060).
