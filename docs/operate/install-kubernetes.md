# Install a Kubernetes cluster

Make a Kubernetes cluster of naru Nodes with its default add-ons: cluster DNS and PersistentVolumes, which every cluster gets unless one is disabled. It ends with a control-plane Node and a worker that answer `kubectl`. It does not cover what aurora owns (ingress, TLS, databases, object storage, secrets) or the path in from outside the cluster. The designs are [ADR 0031](../architecture/adr/0031-a-node-runs-kubernetes-as-init-services-containerd-and-runc-are-static-and-pods-get-routed-prefixes-from-one-pod-network.md), [ADR 0083](../architecture/adr/0083-a-control-plane-node-applies-pinned-cluster-add-ons-from-its-image-with-an-init-service-and-every-node-imports-their-images-so-coredns-and-a-local-volume-provisioner-run-by-default.md) and [ADR 0090](../architecture/adr/0090-a-volume-is-made-where-its-disk-is-by-a-provisioner-per-node-a-disk-the-node-uses-is-never-handed-out-and-an-operator-frees-a-retained-disk-by-deleting-its-volume.md), and [config.md](../architecture/config.md#kubernetes-adr-0031) lists every flag.

`<ca-dir>` is a directory on the operator's workstation that holds authority keys until step 2. `<endpoint>` is the control-plane Node's address, `<pod-network>` the range Pods take addresses from (for example `10.64.0.0/12`) and `<prefix-N>` one Pod prefix per Node inside it. The Service range is `10.96.0.0/16` and cluster DNS is `10.96.0.10`, neither of which a prefix may overlap. The Nodes boot as any naru Node does, from the release ([first-hosts.md](./first-hosts.md) for hosts that place them). `TestNode_KubernetesDNSResolvesAServiceOfItsOwnNamespaceAndOfAnotherAndAnExternalNameThroughTheNodesResolver` (`tooling/qemu/tests/kubernetes_dns_test.go`) and the `TestNode_KubernetesA…Volume…` tests (`tooling/qemu/tests/kubernetes_volumes_test.go`) run steps 1 to 5, except that the harness writes the extension image into the data disk's store before the first boot, where step 3 delivers it over the Node API.

## 1. The cluster's authorities

```
naructl gen ca --ca-dir <ca-dir>
naructl gen api-ca --ca-dir <ca-dir>
naructl gen kubernetes --ca-dir <ca-dir> --endpoint <endpoint>
naructl gen api-client --ca-dir <ca-dir> --role admin --endpoint <endpoint> --out <ca-dir>/admin-api.yaml
```

`gen api-ca` makes the Node API's authority; with it in `<ca-dir>`, `gen kubernetes-node` gives each Node a Node API, and `<ca-dir>/admin-api.yaml` is the `<client file>` of step 3 (`--endpoint` names another Node when you pass it to a command). Step 3 needs the API.

The second writes the front-proxy authority, the ServiceAccount key pair and the administrator's `admin.conf` (mode `0600`). Add `--disable-addon dns` or `--disable-addon volumes` (repeatable) to leave an add-on out; the file it writes, `<ca-dir>/disabled-addons`, is read by step 2, so run this step before it. A later `gen kubernetes` without the flag turns every add-on on again. The cloud-controller add-on (`LoadBalancer` Services) is off by default; turn it on with `naructl gen kubernetes --enable-addon cloud-controller` ([ADR 0093](../architecture/adr/0093-the-cloud-controller-add-on-is-opt-in-and-is-the-clusters-load-balancer-implementation-and-one-switch-turns-it-and-the-kubelets-external-provider-on-together-amending-adr-0085-items-1-9-and-12.md)); make its credential and Secret first, or every Node stays tainted.

## 2. The Nodes' user-data

```
naructl gen kubernetes-node --ca-dir <ca-dir> --name node-1 --role control-plane --address <endpoint>/24 --endpoint <endpoint> \
  --pod-network <pod-network> --pod-prefix <prefix-1> --peer <node-2 address>=<prefix-2> --out node-1.user-data
naructl gen kubernetes-node --ca-dir <ca-dir> --name node-2 --role worker --address <node-2 address>/24 --endpoint <endpoint> \
  --pod-network <pod-network> --pod-prefix <prefix-2> --peer <endpoint>=<prefix-1> --out node-2.user-data
```

Give each Node a `--peer <address>=<prefix>` for every other Node. Only the control-plane Node's user-data carries the cluster authority's key; keep both files at mode `0600`. Every Node's user-data makes its kubelet use cluster DNS and imports the add-on images into containerd.

## 3. Storage for PersistentVolumes

The `naru-volumes` add-on is a system extension delivered with the release as `naru-volumes-ext-<tag>.sqfs`.

- A Node that takes a volume gets a disk from `gen kubernetes-node --storage <name>=<label, /dev/path or serial:NUMBER>`: its first disk holds the local volumes of the default class `naru-local`, formatted as XFS with project quota.
- Whole disks for raw block volumes are given with `--block-serial <serial>` (class `naru-block`); a disk the Node itself uses is never handed out.
- Every Node booted with the add-on enabled lists the extension `naru-volumes` with no digest, and init mounts it from the Node's extension store on the data disk. A new Node's data disk is blank, so its store has no image and its first boot is refused with `extension "naru-volumes": no image of "naru-volumes" is stored`. Boot it anyway, then deliver the image: a boot refused after the Node API has started leaves the API serving, and it accepts `PUT /v1/extensions` for this purpose ([init.md](../architecture/init.md)).

```
naructl extension add naru-volumes-ext-<tag>.sqfs --config <ca-dir>/admin-api.yaml --endpoint <Node address>
naructl reboot --config <ca-dir>/admin-api.yaml --endpoint <Node address>
```

The image is the release's, for the tag the Node runs; an image for another version is refused at the next boot. Do it for every Node, the worker too. The reboot finds the image and the boot goes on. A Node that is upgraded takes the image from the upgrade bundle instead ([upgrade-a-host.md](./upgrade-a-host.md)).

## 4. Boot and wait

Boot the control-plane Node first, then the worker, each with its user-data and the data disk it needs; with the volumes add-on on, deliver the image to each as step 3 says and reboot it. The control-plane Node starts etcd, the API server, the controller manager and the scheduler, and its one-shot service `addons` applies the add-ons' manifests until the API server answers.

## 5. Check the cluster and the add-ons

Move `<ca-dir>`'s authority keys to the secret store once both user-data files are written (step 2), and keep `admin.conf` and `admin-api.yaml`.

```
kubectl --kubeconfig <ca-dir>/admin.conf get nodes
kubectl --kubeconfig <ca-dir>/admin.conf -n kube-system get pods
```

- Both Nodes are `Ready`.
- DNS: the CoreDNS Pod runs in `kube-system`, and a Pod resolves a Service name of its own namespace and of another, and an external name through the Node's resolver.
- Volumes: a claim of the default class binds when its first Pod is scheduled, the Pod writes to it, and after the Pod is deleted a new Pod on the same Node finds the data. A Pod on the other Node cannot start with a `naru-local` claim, since the volume lives on the Node whose disk made it.
- A `naru-block` volume is released when its claim is deleted and stays `Released`; setting the volume's `persistentVolumeReclaimPolicy` to `Delete` frees the disk, and the driver never wipes it ([ADR 0090](../architecture/adr/0090-a-volume-is-made-where-its-disk-is-by-a-provisioner-per-node-a-disk-the-node-uses-is-never-handed-out-and-an-operator-frees-a-retained-disk-by-deleting-its-volume.md)).

## 6. Upgrade

The add-ons are pinned in the release and are applied again by the new release's control-plane Node after an upgrade ([upgrade-a-host.md](./upgrade-a-host.md)); nothing is fetched from the internet.
