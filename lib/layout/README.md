# lib/layout

The Node layout: the one writer of a Node's user-data, in pure Go over `go.yaml.in/yaml/v3`.

- `layout`: a Node's services and their arguments, `spec.host`, mvmd's configuration, the data disk's paths and the daemons' sections, and the configuration orch builds for a joined host from the fleet's profile.
- `layout/pki`: the Node API and host API authorities and the serving and client pairs they issue.

Linked by `naructl`, and by orch to build a joining host's configuration (`docs/architecture/structure.md`, ADR 0060).
