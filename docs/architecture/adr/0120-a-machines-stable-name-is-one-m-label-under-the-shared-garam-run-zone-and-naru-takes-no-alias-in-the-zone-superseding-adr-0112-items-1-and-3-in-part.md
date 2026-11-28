# ADR 0120: a Machine's stable name is one `m-` label under the shared garam.run zone, and naru takes no alias in the zone, superseding ADR 0112 items 1 and 3 in part

> Status: accepted
> Date: 2026-11-28

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#440. It supersedes in part the means of [ADR 0112](./0112-a-machine-is-reached-by-a-stable-name-through-naru-proxy-which-terminates-tls-reads-the-routes-orch-pushes-to-its-host-and-forwards-over-the-mesh-amending-adr-0085-items-3-5-and-6.md):
- item 1's forms, `<id>.m.garam.sh` and `<service>--<id>.m.garam.sh`;
- item 3's "Inside the zone", where a use could publish any label as an alias.

It keeps the outcomes those items state. A Machine has a name that no move changes, which one wildcard certificate covers and which reveals neither the caller nor the Machine's name, and a use publishes its own names as aliases. The rest of ADR 0112 stands, and its body is not edited.

## Context

- **User content leaves garam.sh.** sauce bought garam.run for everything users and bots make, and garam.sh keeps only garam's own services and sign-in (#440). A Machine's address is user content.
- **Only one level of names under garam.run** (the coordinator, 2026-11-27, on #440).
  - Every name is one label under `garam.run`, and a prefix marks whose it is.
  - Machines are `m-*`: their records are DNS-only, since naru terminates their TLS.
  - cowork's sites are `c-*`, proxied by Cloudflare.
  - `garam.run` alone goes on the Public Suffix List, so every name under it is its own site for cookies and storage.
- **The zone is shared.** cowork's names, and user sites later, live beside the Machines' names. A label a user chose would invite name squatting and lookalikes, and the policy for such names belongs to garam.run as a whole, not to naru.
- **The PM's decision on #440 (2026-11-28).** In garam.run, naru answers only `m-<id>.garam.run` and `m-<service>--<id>.garam.run`. There are no aliases in the zone; aliases remain only as granted domains outside it. In-zone aliases may come back under a garam.run naming policy. The zone's certificate stays a configured file until naru's own ACME (#395).

## Decision

1. **The forms.** A Machine's default service is `m-<id>.<zone>`, and its other services are `m-<service>--<id>.<zone>`.
   - `<id>` is the Machine's 16-hex-digit `id`, as before.
   - The prefix `m-` is fixed (`lib/machineapi.MachinePrefix`), not configured.
   - The zone is orch's `--machine-zone` (`spec.orch.machineZone`): `garam.run` for the coordinator's fleet.
   - Each name is one label, so the zone's wildcard certificate, `*.garam.run`, covers every one.
   - A service's name is at most 43 characters, so its name stays one label of 63; a longer one is 422 on the spec.
2. **naru takes no alias in the zone.**
   - `PUT /machines/v1alpha1/aliases/{hostname}` refuses a hostname in the zone, or the zone itself, with 422 `invalid`. A stable name in its new form is refused the same way.
   - An alias is a hostname in a domain the operator granted the caller (`PUT /v1/callers/{name}` with `aliasDomains`), with its own certificate and key, as ADR 0112 item 3's "Outside the zone" decides.
   - Aliases in the zone may come back under a naming policy of garam.run as a whole, which this record does not set.
3. **The zone's certificate is a file until #395.** `spec.proxy.certificate` holds `*.garam.run` and its chain, and the key is a `spec.files` entry, as ADR 0112 decides. naru's own ACME, DNS-01 on the garam.run zone with a token scoped to that zone, comes with #395.
4. **What naru does not run.** The zone's DNS, its `m-*` records, DNS-only, and the Public Suffix List entry for `garam.run` are the site's.

## Options considered

- **Keep two levels, `<id>.m.garam.run`.**
  - The zone would be naru's alone, and in-zone aliases could stay.
  - Rejected. The coordinator allows one level under garam.run, and a wildcard covers one level only.
- **A configured prefix.**
  - A second fleet could share the zone under another prefix.
  - Rejected. No second fleet exists, and a prefix differing per member would split the names. The zone is already per cluster, and a prefix can be added the day a second fleet needs it.
- **Aliases in the zone, refused only when they begin with `m-` or `c-`.**
  - Uses could keep publishing short names.
  - Rejected by the PM. Any other label is still a user's choice in a shared zone, open to squatting and lookalikes, and it would decide a policy for garam.run inside naru.

## Consequences

- A use that published an alias in the zone now asks the operator for a domain and brings a certificate for it.
- A cluster records its zone when its first member serves, and refuses the Machine API on a member started with another zone (machines.md). So a cluster runs on the zone it started with. Moving a running cluster from `m.garam.sh` to `garam.run` is not built: a new cluster is started on `garam.run`.
- An alias stored in the zone before this record stays routed until its use or its Machine deletes it.

## Signs the decision was wrong

- garam.run gets a naming policy for user-chosen labels. Then in-zone aliases should come back under it.
- A second fleet must share garam.run. Then the prefix should become the cluster's, recorded as its zone is.
