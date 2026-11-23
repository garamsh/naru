# Reach a member's API server from the LAN

Run `kubectl` from a workstation on the LAN against the API server of a Kubernetes Node that is a microVM of a network, through a LAN address one host holds for it. It does not cover Services of type LoadBalancer, whose addresses the cluster's controller asks for ([ADR 0085](../architecture/adr/0085-a-service-of-type-loadbalancer-is-fulfilled-by-a-naru-cloud-controller-manager-that-asks-orch-for-an-address-from-a-pool-one-host-announces-it-on-the-lan-and-forwards-it-to-the-nodes-nodeports.md)), or a Node that is a host itself. The design is [ADR 0102](../architecture/adr/0102-the-operator-declares-an-exposure-of-a-networks-members-at-any-backend-port-so-the-leased-lan-address-reaches-a-members-api-server-and-an-owner-table-holds-the-exposer-to-the-nodeport-range-amending-adr-0085-items-4-6-and-7.md).

Values in angle brackets are yours. The example uses the pool `192.168.0.112/29` (`.112` to `.119`), which the LAN's owner reserved for naru, and the address `192.168.0.112` for the member `cp1-1` of the network `lab`. The orch commands take `--orch-url`, `--orch-ca`, `--identity` and `--passphrase-file`, as in [first-hosts.md](./first-hosts.md). `TestFleet_TheOperatorDeclaresALANAddressForAPortOutsideTheNodePortRangeAndAnExposerNeitherMakesNorSeesIt` (`tooling/qemu/tests/exposure_fleet_test.go`) runs steps 1 and 2 and the LAN client's reach of the port; the part that names the address in the certificate is `TestKubernetesUserData_TheAPIServersCertificateNamesTheServingAddresses` (`lib/layout/kubernetes_test.go`).

## Before you start

- The hosts that run the member announce the address on a LAN interface: `naru-hostd` takes `--lan-interface=<interface>`, which needs `--management-prefix`, and without it no host is a candidate and the exposure stays `pending`. The configuration has no field for it; the fleet test adds the argument to the `naru-hostd` service's `args` in the user-data.
- The pool's range is on the hosts' LAN segment, inside the LAN interface's subnet, with no DHCP in it and no other user, and overlaps no network's prefix and no other pool.
- The network and the group of the member exist, and the group's template lists the member's address ([groups.md](../architecture/groups.md), [ADR 0099](../architecture/adr/0099-a-group-template-lists-its-members-addresses-by-number-orch-places-a-member-on-a-host-whose-machine-prefix-holds-its-address-and-mvmd-gives-it.md)). `<member address>` below is that address, such as `10.200.1.10`.

## 1. The pool

```
naructl address-pool apply -f pool.yaml
```

```yaml
name: api
cidrs: [192.168.0.112/29]
networks: [lab]
```

It prints `naructl: address pool api applied: 192.168.0.112/29 for lab`.

## 2. The exposure, before the certificate

The address is chosen now because the API server's certificate names it and is signed before the Node exists.

```
naructl exposure apply -f api.yaml
```

```yaml
id: api
network: lab
pool: api
requestedAddress: 192.168.0.112
ports: [{protocol: TCP, port: 6443, backendPort: 6443}]
backends: [<member address>]
```

It prints `naructl: exposure api of network lab applied: address 192.168.0.112, pending`. Orch refuses (409) an address another exposure holds, (422) an address outside the pool or a backend that is not the address of a live member of `lab`, and (403) a pool that does not list `lab`. The exposure is the operator's: the cluster's controller neither lists nor releases it.

## 3. The user-data and the kubeconfig

```
naructl gen kubernetes --ca-dir <ca-dir> --endpoint <member address> --admin-address 192.168.0.112:6443
naructl gen kubernetes-node --ca-dir <ca-dir> --name cp1-1 --role control-plane --address <member address>/<bits> --endpoint <member address> \
  --pod-network <pod-network> --pod-prefix-from-seed --serving-address 192.168.0.112 --out cp1-1.user-data
```

`--serving-address` adds the address to the IP SANs of the API server's certificate, and `--admin-address` makes `<ca-dir>/admin.conf` dial it. A worker takes no `--serving-address`. The remaining flags are as in [install-kubernetes.md](./install-kubernetes.md) step 2.

## 4. Boot and check

Apply the group with this user-data as its template's `userData`, then:

```
naructl exposure get lab api
kubectl --kubeconfig <ca-dir>/admin.conf get nodes
```

The first shows `"phase": "announced"` and the ID of the host that holds the address; the second answers from the LAN workstation.

## What to know

- One host holds the address at a time, the one that runs a backend, and announces it by gratuitous ARP. If that host is lost, the address moves to another host that runs a backend in under 25 s, and open connections reset. There is no second holder.
- The API server's audit log shows the announcing host's address as the client, because the flow is masqueraded. Clients are told apart by their certificates.
- The exposer credential cannot make this exposure: orch refuses it any backend port outside 30000 to 32767 (403), so the cluster's own controller cannot send the LAN to a Node's API server or kubelet.
- To change the address, delete the exposure (`naructl exposure delete lab api`; the address is free once its host confirmed the removal or the lease and skew passed), declare it again with the new address, and generate the Node's user-data again: the certificate names the old one.
