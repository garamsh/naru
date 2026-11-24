// Package machineapi holds the request and response types of naru's Machine API, machines/v1alpha1,
// and a Go client for it. The specification is docs/architecture/machines.md §The API, with these
// types; the conformance test is the package machineapitest and the in-memory implementation is
// machineapifake (ADR 0108 item 2).
//
// # Compatibility
//
// The version is Version, served under PathPrefix. Within it:
//
//   - Adding an operation, an optional field, a capability, a phase or an error code keeps the
//     version.
//   - Removing or renaming anything, or changing what an operation means, its idempotency or its
//     state after a failure, is a new version.
//   - A deprecated element is retired only by a new version.
//   - Alpha versions are served one at a time: a server of machines/v1alpha2 serves no v1alpha1.
//   - The first stable version, v1, needs a second production implementation that passes
//     machineapitest; until then the version stays below v1.
//
// So a client meets additions it does not know. This one reads an unknown phase as PhaseUnknown, an
// unknown capability value as absent, an unknown field as nothing and an unknown error code as the
// string it is.
package machineapi
