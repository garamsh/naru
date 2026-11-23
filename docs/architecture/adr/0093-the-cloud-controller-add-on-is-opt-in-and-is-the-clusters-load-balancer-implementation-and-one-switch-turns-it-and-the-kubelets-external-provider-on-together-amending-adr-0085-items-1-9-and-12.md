# ADR 0093: the cloud-controller add-on is opt-in and is the cluster's LoadBalancer implementation, and one switch turns it and the kubelets' external provider on together, amending ADR 0085 items 1, 9 and 12

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#275, part of goal garamsh/naru#262. It amends [ADR 0085](./0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md) items 1, 9 and 12 by reference: no body is edited, and what that ADR says holds except where this one says otherwise. The default and the setting it replaces were decided with the PM in the dispatch of this change (2026-11-23).

## Context

- **ADR 0085 item 12 reads as a default-on add-on with an opt-out**, as ADR 0083 item 12 has for DNS and volumes: `addons: {cloudController: false}` and `addons: {loadBalancer: false}`.
- **The add-on cannot work without a credential an operator makes.** Its node controller and its service controller both call orch with an `exposer` pair ([ADR 0085](./0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md) items 7 and 9), which item 7 says the operator hands to the cluster as a Secret and no Node or orch creates.
- **The kubelet flag makes an absent controller a broken cluster.** With `--cloud-provider=external` every Node keeps the taint `node.cloudprovider.kubernetes.io/uninitialized` until a cloud-controller-manager initializes it. A cluster that turned the add-on on by default would have every Node unschedulable until an operator ran `naructl ccm credential`, and an existing cluster upgraded to this release would stop scheduling.

## Decision

1. **The add-on is named `cloud-controller` and is off by default.** `naructl gen kubernetes --enable-addon cloud-controller` turns it on; the flag is repeatable, accepts only the opt-in add-ons (`lib/layout`'s `OptInAddons`), writes `<ca-dir>/enabled-addons`, and a later `gen kubernetes` without it removes the file, so the add-on is off again, as `--disable-addon` behaves for the add-ons that are on by default. `--disable-addon` and the file it writes are unchanged and name only the add-ons that are on by default.
2. **One switch decides both halves.** The add-on on gives every Node's kubelet `--cloud-provider=external`, makes a control-plane Node apply the add-on's manifests, and has every Node import its image. The add-on off gives none of the three, so an existing cluster is unchanged by the release that carries it.
3. **There is no node-controller-only setting.** ADR 0085 item 12's `loadBalancer: false` is not built: the service controller can be turned off in the cloud-config the operator's Secret holds (`"loadbalancer": false`), which is the credential's own file and not a cluster setting of `naructl`, until someone needs a setting the Nodes carry.
4. **The image carries the binary once.** The add-on's image archive holds `naru-ccm`, and the initramfs holds the archive and no copy beside it; the add-on is delivered in the initramfs, not as a system extension ([ADR 0084](./0084-an-upgrade-bundle-carries-the-extensions-the-hosts-configuration-names-and-an-entry-without-a-digest-is-resolved-by-init-to-the-image-built-for-its-version.md) is not used for it), because the stored sizes stay under the 256 MiB of [ADR 0083](./0083-a-control-plane-node-applies-pinned-cluster-add-ons-from-its-image-with-an-init-service-and-every-node-imports-their-images-so-coredns-and-a-local-volume-provisioner-run-by-default.md)'s last section (the pull request that builds it states the sizes). If a later add-on or a larger binary takes the sum past it, the delivery is an extension.

5. **`naru-ccm` is the cluster's LoadBalancer implementation, so a Service's `loadBalancerClass` is not a naru setting.** [ADR 0085](./0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md) item 1 has the controller fulfil a Service with no class or the class `naru.garam.sh/host`. The upstream service controller `naru-ccm` runs treats a Service with any `spec.loadBalancerClass` as another implementation's and never asks the provider for it (`k8s.io/cloud-provider` `wantsLoadBalancer`), so the class cannot be fulfilled without building a class-aware controller. That is not built: since the add-on is opt-in, the operator who turns it on chooses `naru-ccm` as the cluster's implementation, and the class `naru.garam.sh/host` is dropped. The provider answers for every Service the service controller hands it, whatever it is called.
6. **A Node's name is a group member's name, and `lib/layout` refuses one that is not.** [ADR 0085](./0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md) item 9 has the implementation check the Node's name against `lib/layout`'s hostname and, if it differs, add the mapping to the cloud-config file. The kubelet's hostname is the Node's name (`--hostname-override`), which the node controller looks up as a member's name, and the member's name is `<group>-<number>` (orch's `groups`). With the add-on on, `lib/layout` refuses a Node whose name has not that form. The mapping in the cloud-config is not built: a name that differs is refused when the cluster's configuration is written, not mapped at run time.

## Options considered

- **On by default with the credential made by the install.** A Node or orch would hold or make an `exposer` pair, which ADR 0085 item 7 refuses: it is the credential whose strength the decision's boundary rests on.
- **On by default with the controller tolerating a missing Secret.** The Nodes would stay tainted all the same, since only an initialized Node loses the taint; the failure would be a cluster that looks healthy and schedules nothing.
- **A class-aware controller that fulfils `naru.garam.sh/host` beside others.** It means a second service controller or a fork of the upstream one, to let a cluster run `naru-ccm` beside another implementation, which nobody asked for.
- **A node-controller-only switch now.** It doubles the settings of a first release for a case nobody asked for.

## Consequences

- A cluster that never enables the add-on pays nothing for it at run time: no kubelet flag, no Deployment, no image imported. The initramfs carries its archive.
- An operator who enables it makes the credential first (`naructl ccm credential`), creates the Secret `naru-ccm` in `kube-system`, and then writes the cluster's configuration with the flag; the order is the operator's, and the Nodes wait, tainted, until the controller runs.
- Turning the add-on on or off on a running cluster is a Node reconfiguration and a reboot, as ADR 0085 item 12 says of the flag.

## Signs the decision was wrong

- Every cluster aurora runs enables it: then it is on by default with the credential made by whoever installs the cluster, which is a decision about item 7.
- A cluster needs the node controller without the service controller more than once: then the setting of item 3 is a cluster setting.
