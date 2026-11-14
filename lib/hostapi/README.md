# lib/hostapi

The request and response types of the host API that `naru-hostd` serves, and a Go client for it.

- `hostapi.NewClient(base, httpClient)` returns a client; the caller's `http.Client` carries its mTLS identity.
- A refusal or failure is a `*hostapi.Error` carrying the status, the message and the fields it names.

Linked by `host`, which serves these types, and by `orch`, which calls them.
