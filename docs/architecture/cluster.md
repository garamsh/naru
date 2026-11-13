# Cluster API

What `naru-clusterd` is: the controller manager that offers naru as Kubernetes resources, keeps an inventory of hosts, and turns a declared cluster into Cluster API objects. It is the interface a customer or a tool above naru declares clusters through; the controllers that make Cluster API's provider objects real are other work.

## Current decisions

- **`naru-clusterd` is a unit.** It is the module `cluster/`, with one binary, `cmd/naru-clusterd`. It imports no other unit and reaches a host only through the host API's wire format ([host.md](./host.md)), which it replicates and does not import.
- **One API group, `naru.garam.sh/v1alpha1`.** The CRDs are generated from the Go types, committed under `cluster/config/crd/bases`, and checked current by `make ci`.

  | Kind | Scope | What it is |
  | --- | --- | --- |
  | `NaruHost` | cluster | One host: the host API's address and the Secret of the client pair; status is the region, labels, capacity and free CPU, memory and disk the host last answered, and a `Ready` condition. |
  | `NaruClusterDeclaration` | namespaced | One customer cluster: `region`, `hostSelector`, `controlPlane.replicas` (1 or 3), `workers.replicas`, `version`, `node.cpu` and `node.memory`. |
  | `NaruCluster`, `NaruMachine`, `NaruMachineTemplate`, `NaruConfig`, `NaruConfigTemplate`, `NaruControlPlane` | namespaced | The provider kinds of Cluster API's contract at contract version v1beta2. |

- **A host is polled.** `Ready` is `True` (`Answered`) while the host API answers, and `False` with `Unreachable`, `Rejected` (the host API's 403) or `ClientPairInvalid` otherwise. An unreachable host keeps its last known status and is asked again at the poll interval (30 s by default).
- **A declaration creates seven objects, each owned by it:** `NaruCluster`, `NaruControlPlane` and `Cluster` named as the declaration; the control plane's `NaruMachineTemplate` as `<name>-control-plane`; the workers' `NaruMachineTemplate`, `NaruConfigTemplate` and `MachineDeployment` as `<name>-workers`. Changing replicas, version or the host selector updates them. The declaration's name is at most 49 characters.
- **Deletion goes through a finalizer.** Deleting a declaration deletes its `Cluster`, and the declaration is released when the `Cluster` is gone; Cluster API's deletion takes the rest. An object of a built name that the declaration does not control is never touched.
- **`region` and `node` cannot change**, and neither can a `NaruMachineTemplate`'s template. Admission is the CRDs' validation; no webhook runs.
- **Tests run on envtest** with this unit's CRDs and Cluster API's installed. `make envtest` fetches the pinned binaries, verified by SHA-256; the tests use no network. The pinned versions are in the ADR.

## Rationale

- [ADR 0033](./adr/0033-naru-clusterd-offers-naru-as-kubernetes-resources-and-turns-a-cluster-declaration-into-cluster-api-objects-tested-on-envtest.md): why a unit of its own, why these kinds and this contract version, why a polled host with a condition and not an error, why the finalizer, why the immutability, and how the versions and the envtest binaries are pinned.
- [ADR 0028](./adr/0028-naru-hostd-serves-a-host-api-over-mutual-tls-from-its-own-authority-and-creates-microvms-through-mvmds-runtime-api.md): the host API a `NaruHost` reads.

## Open questions

- **The provider kinds have no controller.** A `Cluster` made from a declaration does not provision until the infrastructure, bootstrap and control-plane controllers exist.
- **Resizing.** `node` is fixed for the life of a declaration. A rolling change of template is not decided.
- **A host that never answers.** A `NaruHost` that stays `Unreachable` is only reported; what placement does with it is for the provider controllers.
- **Cluster API's webhooks** are not run in the tests, so what a full Cluster API would refuse of the built objects is not shown.
