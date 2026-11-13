# ADR 0033: naru-clusterd offers naru as Kubernetes resources and turns a cluster declaration into Cluster API objects, tested on envtest

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#99. It builds on [ADR 0028](./0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md), whose host API is the only way this unit reaches a host, and amends neither it nor [ADR 0001](./0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md).

## Context

- **naru has no way to say "a cluster" yet.** A host creates microVMs one at a time over its host API. What a customer wants is a Kubernetes cluster of a size in a region, and something has to turn that into microVMs on hosts chosen for it.
- **Cluster API is the vocabulary the rest of the platform speaks.** A `Cluster`, a control plane, and `MachineDeployment`s of workers are objects other tools already read. Cluster API names a contract for each kind a provider supplies, and a provider that follows the contract needs no change in the core controllers.
- **A Kubernetes controller needs a real API server to be tested.** A fake client accepts what an API server refuses (validation rules, owner-reference rules, status subresources) and so passes tests that the product fails.
- **The unit must stay independent.** [ADR 0001](./0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md) makes each unit its own module, and [ADR 0028](./0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md) makes the host API's wire format the interface to a host.

## Decision

1. **The unit is `cluster/`, a module `github.com/garamsh/naru/cluster` with one binary, `cmd/naru-clusterd`.** The name follows `host/` and `naru-hostd`: it is named for what it runs (clusters) and not for a technical role such as "operator" or "controller". It imports no other unit and reaches a host only through the host API's wire format, which it replicates in `internal/host` and does not import from `host/internal/api`. The Go types live in `internal/`: the CRDs are the published surface.
2. **One API group, `naru.garam.sh/v1alpha1`**, with these kinds:
   - `NaruHost` (cluster-scoped): `spec.address` (host:port of the host API) and `spec.clientSecretRef` (a Secret in a namespace). Its status holds `region`, `labels`, `capacity` and `free` (CPU, memory, disk) as the host API's `GET /v1/host` last answered, and a `Ready` condition.
   - `NaruClusterDeclaration` (namespaced): `region`, `hostSelector` (a label selector), `controlPlane.replicas` (1 or 3), `workers.replicas`, `version`, and `node.cpu` and `node.memory` (the size of every Node). Its status holds `observedGeneration` and a `Reconciled` condition.
   - The provider kinds `NaruCluster`, `NaruMachine`/`NaruMachineTemplate`, `NaruConfig`/`NaruConfigTemplate` and `NaruControlPlane`, each following Cluster API's contract at **contract version v1beta2** (`cluster.x-k8s.io/v1beta2: v1alpha1` on each CRD). This decision defines the types and nothing that reconciles the provider kinds: their controllers are other decisions.
3. **A `NaruHost` is polled, and an unreachable host is a state.** The controller reads the client pair from the Secret (`tls.crt`, `tls.key`, and `ca.crt`, the authority that signed the host's serving certificate), asks `GET /v1/host` over mutual TLS 1.3, writes the answer to the status, and asks again after the poll interval (30 s unless `--host-poll-interval` says otherwise). `Ready` is `True` with reason `Answered`, or `False` with `Unreachable` (no usable answer), `Rejected` (the host API's 403: the certificate carries no role) or `ClientPairInvalid` (the Secret is missing or does not hold a usable pair). A host that does not answer keeps the last region, labels and capacity it had, and is not a reconcile error: back-off would only delay the next look.
4. **A declaration creates seven objects, each owned by it** (controller owner reference), in the order the later ones refer to the earlier: `NaruCluster` and `NaruControlPlane` named as the declaration, the control plane's `NaruMachineTemplate` (`<name>-control-plane`), the `Cluster` (named as the declaration, referring to the `NaruCluster` and the `NaruControlPlane`), and the workers' `NaruMachineTemplate`, `NaruConfigTemplate` and `MachineDeployment` (`<name>-workers`). A change of `workers.replicas`, `controlPlane.replicas`, `version` or `hostSelector` updates the object that carries it. The declaration's name is at most 49 characters, by a validation rule, so the suffix keeps every name a valid label value.
5. **`region` and `node` cannot change, and a `NaruMachineTemplate`'s template cannot change.** The validation rule is `self == oldSelf` in the CRD. A new size is a new declaration: Cluster API's way to change a template is a new template and a changed reference, which the declaration's one-template-per-pool shape would turn into a roll-out the unit does not yet decide.
6. **The declaration holds a finalizer, `naru.garam.sh/cluster`.** On deletion the controller deletes the `Cluster` it controls and removes the finalizer when the `Cluster` is gone; Cluster API's own deletion takes the rest down. Owner references alone would make a garbage collector delete the `Cluster`, but only the finalizer makes the declaration say it is gone when the `Cluster` is, and makes the deletion observable where no garbage collector runs (envtest). An object of one of the seven names that the declaration does not control is never changed, adopted or deleted: the declaration reports `Reconciled=False`, reason `ApplyFailed`.
7. **Admission is the CRD's validation, not a webhook.** `controlPlane.replicas` is 1 or 3, `region` is a DNS label of at most 63 characters, `version` is a Kubernetes version, and `workers.replicas` is 0 or more. Cluster API's webhooks are not part of this unit.
8. **Tests run on envtest, with no fake client for what envtest can run.** `internal/testenv` starts one API server per test package with this unit's CRDs and Cluster API's core CRDs installed, and the tests make a manager of the controllers under test on it. The host API of a `NaruHost` test is an `httptest` server with mutual TLS 1.3 from a throwaway authority made in memory. A test run without `KUBEBUILDER_ASSETS` fails and does not skip.
9. **Versions, each at least two weeks old on the day of this decision:**

   | What | Version | Released |
   | --- | --- | --- |
   | `sigs.k8s.io/controller-runtime` | v0.24.1 | 2026-05-11 |
   | `sigs.k8s.io/cluster-api` and `sigs.k8s.io/cluster-api/api` | v1.14.3 | 2026-10-06 (v1.14.4, 2026-11-03, is too new) |
   | `sigs.k8s.io/controller-tools` (controller-gen) | v0.23.0 | 2026-10-14 |
   | `k8s.io/api`, `apimachinery`, `client-go` | v0.37.0 | 2026-08-26 |
   | envtest binaries (kube-apiserver, etcd) | `envtest-v1.37.0` | 2026-08-27 |

   controller-gen is a `tool` of the module and runs as `go tool controller-gen`. The CRDs and the deepcopy functions are generated and committed; `make ci` regenerates them in a copy and fails if they differ (`make crds-check`). The root module `sigs.k8s.io/cluster-api` is required only because its CRD files, which the API module lacks, are the ones envtest installs; a blank import in `internal/testenv` keeps it in `go.mod`.
10. **How envtest gets its binaries.** `make envtest` downloads the controller-tools release asset `envtest-v1.37.0-linux-amd64.tar.gz`, verifies its SHA-256 (`f03faedfc68d2a78cee5cb02618bb44e7f67044f4c5fc957f72d3307fda61a9c`) against the one in the Makefile, extracts it under `build/envtest/`, and `make test` exports `KUBEBUILDER_ASSETS` to that directory. The fetch is a step of `make ci`; the tests use no network when they run. Only linux/amd64 is pinned.

## Consequences

- A caller declares a cluster in one object and gets Cluster API's objects for it, owned and deleted with it. Until the provider kinds have controllers (other decisions), the `Cluster` does not provision: the objects exist and nothing yet makes them true.
- The unit's CRDs are the interface. A change to a Go type is a change to a committed CRD, and `make ci` refuses one without the other.
- `make ci` needs the network once, to fetch the envtest binaries and the modules, and a Linux amd64 machine for the binaries. A developer on another platform has no pinned binaries.
- Cluster API's webhooks, which default and validate Cluster API's own kinds, do not run in the tests: the tests prove what this unit sets, not what a full Cluster API would then accept.
- A declaration whose size must change is replaced, not edited.
- A `NaruHost` is polled by one replica and at one interval for all hosts; a large inventory of hosts costs a request per host per interval.

## Signs the decision was wrong

- Customers must resize a running cluster's Nodes: the immutability of `node` and of the templates is then the cost, and the declaration needs rolling templates.
- A provider controller needs to know that a host went away before the poll finds it, or the poll interval is seen to leave a host's capacity too stale to place on: the host should push, or the interval should be per host.
- The envtest binaries are needed on a platform other than linux/amd64, or the release assets stop being published: the fetch needs another source.
- Cluster API's webhooks are found to refuse an object this unit builds, which the tests could not show: the tests need the webhooks, or the objects need what the webhooks require.
- The seven names collide with objects people already have in the namespace, often enough that the refusal to adopt is more cost than care: the names need a prefix or a label-based lookup.
