# lib/metrics

The Prometheus registry of a naru daemon and the listener that serves it, over `prometheus/client_golang`.

- `metrics.New()` returns the daemon's registry, with the Go and process collectors; a daemon makes it once, at its composition site.
- `metrics.Serve(ctx, addr, reg)` serves it at `/metrics` on plain HTTP until `ctx` ends.
- A daemon calls `Serve` on its `--metrics-addr` and serves nothing when the flag is empty. A daemon defines its own metrics with `client_golang` types and registers them on the registry.

Linked by the daemons of naru (`docs/architecture/structure.md`).
