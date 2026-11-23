# ADR 0107: an exposed port's protocol is one type, spelled TCP or UDP, in lib/wire, that orch's API, the host API and hostd share, amending ADR 0085 items 4 and 6

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#365, part of goal garamsh/naru#262. It amends items 4 and 6 of [ADR 0085](./0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md) by reference, and adds a value to the shared constants of [ADR 0070](./0070-shared-constants-are-defined-once-in-the-leaf-module-lib-wire-one-rule-says-when-a-disk-is-blank-and-a-test-parses-what-the-layout-writes.md).

## Context

- **Orch and hostd spelled the protocol differently.** On goal/262 at b4f5429, orch's spec validation took only `TCP` and `UDP` (`orch/internal/exposures/exposures.go`, `validateSpec`), `naru-ccm` sent those, orch's leader passed the value unchanged to the host (`orch/internal/hostclient/exposurehosts.go`, `Put`), and hostd took only `tcp` and `udp` (`host/internal/network/exposure.go`, `TCP` and `UDP`; `host/internal/api/exposure.go`, `parseExposure`). Every lease orch asked for was refused, so no Service of type LoadBalancer and no member API exposure got an address (aurora at d93cfa7, garamsh/naru#365).
- **Each side was a `string` in its own package.** `lib/orchapi.ExposurePort.Protocol` and `lib/hostapi.ExposurePort.Protocol` were plain strings and hostd had its own `network.Protocol`, so nothing compiled the two spellings against each other.
- **The batch log shows the refusal.** `~/.cache/naru-pm/snap-d93cfa7/batch/4a-exposure.log` holds 145 answers of 422 to `PUT /v1/exposures/10.78.0.80`, each logged by orch as `host API 422: the request is invalid (ports[0])`.
- **The orch API's spelling is documented to operators.** `docs/operate/reach-a-members-api-server.md` writes `protocol: TCP`, and Kubernetes spells a Service port's protocol `TCP` and `UDP`. The host API's spelling is read only by orch.

## Decision

1. **One type, `wire.Protocol`, with the two values `TCP` and `UDP`**, in `lib/wire` with `Protocols()` and `Valid()`. `lib/orchapi.ExposurePort`, `lib/hostapi.ExposurePort` and hostd's stored `network.ExposurePort` carry it, so orch's API, the orch→hostd client and hostd name the protocol with one type.
2. **The spelling is `TCP` and `UDP`, everywhere.** Orch's API keeps the spelling its documents give, and the host API changes to it: hostd refuses `tcp` and `udp` with 422 naming the port, as any other value (amends item 6). Nothing folds case: a spelling outside `Protocols()` is refused by orch's validation and by hostd's alike (amends item 4).
3. **The nft keyword is hostd's own rendering.** The ruleset writes `tcp` or `udp` from the protocol in one function; it is never read back as a protocol.
4. **A conformance test holds the boundary.** `lib/hostapi/hostapitest.ExposureProtocols` drives a host API implementation through `hostapi.Client` alone: it takes a lease of each of `Protocols()` and lists it in that spelling, and refuses any other spelling with 422 on `ports[0]`. hostd's API runs it; orch's tests show that its validation takes exactly `Protocols()` and that its client sends the value unchanged.

## Options considered

- **Lower case, `tcp` and `udp`.** The orch API documents `TCP` to operators, so it would have to keep taking `TCP` too: two spellings, and a fold in orch.
- **Accept both cases at hostd.** A second spelling carried for good, and the fold in a second place; the interface is pre-stable and its only client is orch.
- **Translate in the orch→hostd client.** Two types would stay, and the next field spelled twice would go the same way.

## Consequences

- A lease hostd stored in the old spelling (`tcp`, `udp`) fails validation when hostd loads its state file, and is dropped at `Warn` with `protocol "tcp"`, the warning of a lease the host's rules no longer accept. Orch renews every lease on every pass (ADR 0085 item 5), so its next pass, within 5 s, puts the lease again in the new spelling and the host takes the address again. aurora's hosts hold such leases from earlier snapshots; they need nothing done.
- A hostd of this change and an orch from before it agree: that orch already sent `TCP`. An orch of this change and a hostd from before it do not, as before this change.
- An operator's file that writes `protocol: tcp` is refused by orch with 400, as before.
- The network policy rules of a microVM (`networkPolicy`, `protocol` of a rule) are mvmd's and keep their own lower-case spelling; they are not an exposure's protocol.

## Signs the decision was wrong

- A caller of orch's API that cannot send the upper-case spelling: it would need a fold, which this decision refuses.
