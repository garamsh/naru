# lib/machineapi

The request and response types of naru's Machine API, `machines/v1alpha1`, and a Go client for it. The specification is `docs/architecture/machines.md` §The API with these types; the package documentation states the compatibility rules (ADR 0108 item 2).

- `machineapi.New(base, httpClient)` returns a client; the caller's `http.Client` carries its caller certificate.
- `Register` spends a one-time registration token for the certificate of a key the caller made, through a client that carries no certificate; `Renew` signs a renewal with the caller's current pair in its last 30 days, once per certificate and for a new key (ADR 0116).
- `signedrequest` is the form of the signed request a renewal is, which `lib/identity` re-exports; it is the standard library's alone, so the module requires no other naru module.
- A refusal or failure is a `*machineapi.Error` carrying the status, the message and the code.
- `Spec.Check`, `SameSpec`, `Spec.Redact`, `AliasInZone` and `CheckAliasCertificate` are the rules every implementation applies, defined once.
- `machineapitest` is the conformance test: it drives an implementation through the client alone.
- `machineapifake` is the in-memory implementation, which passes it, and `machineapifake.Start` serves one over mutual TLS for a caller's tests, with registration tokens from `Token` and an `AnonymousClient` to register with.

Linked by `orch`, which serves the API, and to be required by a use's provider, which calls it as the caller (for garam, garam-bot's workspace provider).
