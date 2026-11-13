# ADR 0031: a Node runs Kubernetes as init services, containerd and runc are static, and each Node's Pods take a routed prefix of one Pod network

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#91, on the critical path of garamsh/aurora#804. It rides on [ADR 0013](./0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md) (init issues the pairs), [ADR 0015](./0015-naructl-is-the-single-owner-of-the-gaon-node-layout-and-tooling-qemu-runs-it-as-a-binary.md) (naructl owns a Node's layout) and [ADR 0023](./0023-spec-network-routes-are-static-ipv4-routes-init-applies-live.md) (static routes).

## Context

- **A Node has no Kubernetes.** The image carries mvmd, gaon and etcd. A cluster whose Nodes are Kubernetes Nodes needs a control plane, a container runtime, a kubelet and a Pod network on the same image, which has no libc and no shell, so every program must be static and every launch is one init service with arguments.
- **The Pod network needs no new component.** [ADR 0023](./0023-spec-network-routes-are-static-ipv4-routes-init-applies-live.md) already routes between Nodes with static routes through netlink. Pod addresses are one more prefix per Node.
- **Init issues certificates from a CA key it carries.** An authority with a `keyPath` is written with its key; a Node can issue only what the authority it carries can sign.
- **The kubelet puts Pod cgroups under a parent of its choice.** Init owns `naru/<service>` and removes a service's leaf on stop.
- **A runtime that needs libseccomp is not static by default.** runc links libseccomp through cgo.

## Decision

1. **Kubernetes runs as init services.** The topology, defined once in `naructl`'s layout (`naructl topology kubernetes`), is `registry`, `containerd`, `etcd`, `kube-apiserver`, `kube-controller-manager`, `kube-scheduler`, `kubelet`, `kube-proxy`, in that order. A control-plane Node runs all of them; a worker runs `containerd`, `kubelet` and `kube-proxy`. The control-plane Node runs a kubelet too, so its Pods run. gaon's topology and `naructl gen node` are unchanged.
2. **Builds.** The image carries kube-apiserver, kube-controller-manager, kube-scheduler, kubelet and kube-proxy from Kubernetes v1.37.2 (commit `c50e5395f1ae2d4a2c05f28fdcc38c0916cc1a32`), built with Go and `CGO_ENABLED=0`; containerd and `containerd-shim-runc-v2` v2.4.2 (commit `b7a25261e029d68978bbf290df0a62fc819b3c51`), built with `CGO_ENABLED=0` and the tags `netgo`, `osusergo`, `static_build`, `no_btrfs`, `no_devmapper`, `no_zfs`; runc v1.5.3 (commit `46365c62967c76a765a5e347f48f0c7b3b571fe9`), built with cgo against a libseccomp v2.6.1 that `image/containerd/build.sh` builds static from the pinned tarball (SHA-256 verified) and linked with `-extldflags -static`; the CNI plugins `bridge`, `host-local` and `loopback` v1.9.1; `nft` from nftables 1.1.7, static and with JSON output, which kube-proxy reads (it needs jansson 2.15.1); and `mount` and `umount` from util-linux 2.42.4, static, which the kubelet runs. The build scripts fail unless every executable is statically linked, and runc unless it reports libseccomp 2.6.1. The pins and their reasons are in `image/kubernetes/`, `image/containerd/`, `image/cni/`, `image/nftables/` and `image/util-linux/`.
3. **Certificates.** A worker never carries the cluster CA's key. On the control-plane Node, `spec.pki` has the authorities `cluster` (certificate at `/run/kubernetes/pki/ca.crt`), `etcd` (the same CA, at gaon's path) and `front-proxy` (made by `naructl gen kubernetes`), and init issues the pairs at boot and renews them: the API server's serving and kubelet-client pairs, the front-proxy client, etcd's two and the API server's etcd client, the controller manager's and the scheduler's, and the Node's own kubelet client (CN `system:node:<name>`, Organization `system:nodes`) and serving pairs and kube-proxy's. The API server's serving pair names the endpoint, `10.96.0.1`, and the `kubernetes.default` names. A worker has no `spec.pki`: `naructl gen kubernetes-node` issues its kubelet client and serving pairs and kube-proxy's from the CA and writes them, with the CA's certificate, as files under `/run/kubernetes/pki`, the form of `--pairs-as-files`. They are renewed by delivering user-data again, and kubelet CSR rotation is not done here.
4. **ServiceAccount keys and the administrator's kubeconfig** are made by `naructl gen kubernetes` (ECDSA P-256) and written into the CA's directory: `sa.key`, `sa.pub`, `front-proxy/` and `admin.conf`. The control-plane Node's user-data carries the ServiceAccount pair as files under `/run/kubernetes/pki` (the key `0600`).
5. **A delegated cgroup.** Init creates `naru/kubernetes`, empty, with `cpu`, `cpuset`, `hugetlb`, `memory` and `pids` enabled for its children (the kubelet refuses a cgroup without them), beside `naru/detached`, and `kubernetes` is a reserved service name. The kubelet uses the cgroupfs driver with `cgroupRoot` `/naru/kubernetes`, so Pod cgroups are under that group and no service's leaf.
6. **The Pod network** is a bridge with host-local IPAM at `/run/cni/net.d/10-naru.conflist`, with `ipMasq` off and the bridge the Pods' default gateway, so a Pod's source address is its own. Each Node's Pods take one prefix of the Pod network, written by `naructl` into `--pod-prefix` and into the bridge's configuration; a route to each other Node's prefix through that Node's address is in `spec.network.routes` (`--peer <address>=<pod-prefix>`). naructl refuses a prefix outside the Pod network, one that overlaps another or the Node's own network, or one that overlaps the Service range `10.96.0.0/16`. kube-proxy runs in nftables mode.
7. **Ports** are the Kubernetes defaults: 6443 (API server), 10250 (kubelet), 10257 (controller manager), 10259 (scheduler), 10256 (kube-proxy health). The kubelet's data is under `/var/lib/naru/kubelet`, containerd's under `/var/lib/naru/containerd`, and the CNI configuration and containerd's socket under `/run`. The image's root is read-only, so nothing else is written.
8. **Images come from the registry service.** `--local-registry` runs the `registry` service on a Kubernetes Node, and `--registry-ca` and `--sandbox-image` make containerd trust it (`/run/containerd/certs.d/127.0.0.1:5000/hosts.toml`) and use the image it serves, pulled by digest, as the Pod sandbox (pause) image.
9. **`naructl gen kubernetes-node`** writes this layout and `gen kubernetes` the cluster's own files ([config.md](../config.md)).
10. **The read-only root is worked around, not opened.** The initramfs links the paths the programs write to into the writable mounts: `/var/run` to `/run`, `/var/log` to `/tmp`, `/var/lib/kubelet` to the kubelet's directory on the data disk and `/var/lib/cni` to `/run/cni`. It links `/etc/passwd`, `/etc/group` and `/etc/hosts` to files a Kubernetes Node's user-data writes under `/run/kubernetes`: the kubelet reads the first two for its user-namespace mappings, and containerd copies the third into each Pod. containerd's NRI is disabled, since it makes its socket under `/var/run`.

## Consequences

- A worker's user-data holds its own pairs and the CA's certificate, never the CA's key, so a compromised worker cannot issue the cluster's identities (aurora#804 decision 10). The control-plane Node's holds the key, as a gaon control-plane Node's does, because init issues its pairs and the controller manager signs with it.
- A worker's pairs are not renewed on the Node, so they expire at the end of the leaf validity unless `naructl apply` delivers new user-data first.
- `naru/kubernetes` is empty and delegated, so what runs in a Pod is not in any service's leaf.
- The Nodes' initramfs holds Kubernetes, so a Node needs more memory than a gaon Node. The QEMU test gives a Kubernetes Node 4096 MiB.
- Routes to Pod prefixes are static and as right as `--peer`, as in ADR 0023.
- The static runc depends on a libseccomp built by the image. A libseccomp release moves with its digest, in one pull request.

## Signs the decision was wrong

- Re-delivering user-data to renew a worker's pairs proves too costly: the kubelet's client pair then rotates through CSRs, which needs the controller manager to approve and sign them.
- The Pod network needs a policy, a Service mesh or more than a flat routed prefix per Node: a CNI plugin then replaces the bridge and static routes.
- The kubelet's Pods need to live in a service's own cgroup, or init needs to account for them: the delegated group is then wrong.
- A Node runs out of memory with the control plane and the runtime on it, in the tests or in a deployment: the control plane then moves to Nodes of its own.
