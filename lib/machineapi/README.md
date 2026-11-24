# lib/machineapi

The request and response types of naru's Machine API, `machines/v1alpha1`, and a Go client for it. The specification is `docs/architecture/machines.md` §The API with these types; the package documentation states the compatibility rules (ADR 0108 item 2).

- `machineapi.New(base, httpClient)` returns a client; the caller's `http.Client` carries its caller certificate.
- A refusal or failure is a `*machineapi.Error` carrying the status, the message and the code.
- `Spec.Check`, `SameSpec`, `Spec.Redact`, `AliasInZone` and `CheckAliasCertificate` are the rules every implementation applies, defined once.
- `machineapitest` is the conformance test: it drives an implementation through the client alone.
- `machineapifake` is the in-memory implementation, which passes it, and `machineapifake.Start` serves one over mutual TLS for a caller's tests.

To be linked by `orch`, which serves the API (#381), and required by garam, whose Workspace calls it.
