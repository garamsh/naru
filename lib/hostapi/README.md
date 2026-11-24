# lib/hostapi

The request and response types of the host API that `naru-hostd` serves, and a Go client for it.

- `hostapi.NewClient(base, httpClient)` returns a client; the caller's `http.Client` carries its mTLS identity.
- A refusal or failure is a `*hostapi.Error` carrying the status, the message and the fields it names.
- `hostapitest` is the conformance test of the host API: it drives an implementation through the client alone (ADR 0107).

- `RoutesRequest` is the routing table orch is to put to a proxy host, and `RoutesFile` what hostd writes from it for naru-proxy (`lib/wire.ProxyRoutesFile`).

Linked by `host`, which serves these types, by `orch`, which calls them, and by `proxy`, which reads the routes file.
