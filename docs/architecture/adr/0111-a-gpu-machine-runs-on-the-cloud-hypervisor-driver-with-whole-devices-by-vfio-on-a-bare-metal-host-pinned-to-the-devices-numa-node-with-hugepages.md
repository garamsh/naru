# ADR 0111: a GPU Machine runs on the Cloud Hypervisor Driver with whole devices by VFIO, on a bare-metal host, pinned to the devices' NUMA node with hugepages

> Status: accepted
> Date: 2026-11-24

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#373 (goal 3). It settles the GPU design input of #373 (comment of 2026-11-23T15:54:33Z, from sauce through cupa): Machines can use GPUs "with performance on a par with GPU use for training". It is designed now and built in a later goal: the first goal's Machines are CPU-only on nested hosts.

## Context

- **mvmd's Drivers.** mvmd runs Machines behind a Driver port. Firecracker v1.17.0 and Cloud Hypervisor v54.0 are its two Drivers, and a host serves the one Driver its configuration names. A Driver reports optional capabilities (`snapshot`, `disk-hotplug`, `cpu-resize`, `memory-resize`, `disk-resize`, `console`), and an operation without its capability is refused as unsupported (garamsh/mvmd `docs/architecture/driver.md` at `2a07ba313c0a2c424ba9badf40ea9c36ec670e14`). Neither Driver attaches a PCI device.
- **Cloud Hypervisor passes whole devices through by VFIO.** Its v54.0 `docs/vfio.md` (tag object `0bd71d82d3dc03408f53c6f61692da00fc44fb6f`) gives a device to the VM with `--device path=/sys/bus/pci/devices/<address>/`. A device bound to `vfio-pci` is passed with every other device of its IOMMU group. "GPUDirect P2P DMA over PCIe" between NVIDIA GPUs is supported "on NVIDIA Turing, Ampere, Hopper, and Lovelace GPUs" through `x_nv_gpudirect_clique`.
- **Firecracker attaches no device.** #373's input records its VFIO as still in development. Firecracker's PCI transport serves virtio devices only (`driver.md` above, `--enable-pci`).
- **Correction to #373's input: Ubicloud runs B200 on QEMU.** The input said Ubicloud runs Cloud Hypervisor with VFIO in production on B200. Ubicloud's post of 2025-12-15 (ubicloud.com/blog/virtualizing-nvidia-hgx-b200-gpus-with-open-source, read 2026-11-24) says otherwise.
  - **What it says.** Ubicloud tried Cloud Hypervisor first, and CUDA failed to initialise on its flat PCI topology. They moved to QEMU with each GPU behind a `pcie-root-port`, because "Cloud Hypervisor does not currently provide a way to construct a deeper, host-like PCIe hierarchy". They also pass HGX GPUs in NVSwitch partitions managed by Fabric Manager.
  - **What it holds for.** It is a fact about HGX B200 with NVSwitch. It is not a fact about a single PCIe GPU, where the Cloud Hypervisor documentation above shows passthrough and P2P on Turing to Lovelace.
  - **What still holds.** Ubicloud's allocator does place GPU VMs on the devices' NUMA node with 1 GiB hugepages (ubicloud/ubicloud `scheduling/allocator.rb` at `8417580759fd085fa20031dde0ba1f6ae1f3152f`).
- **The cost of passthrough.** Bitchebe and Balmau, "MaLV-OS" (arXiv 2508.03676v1, 2025-08-05), measured on 8 V100s with KVM. GPU passthrough "incurs only 13%" overhead "on average, across all workloads and GPU configurations", and up to 37% for image segmentation on 8 GPUs, which they attribute to CPU-side preprocessing under CPU virtualisation.
- **The GPUs in view.** These are consumer GPUs, which offer neither vGPU nor MIG (#373's input), so a device is given whole.
- **OpenNebula.** Hosts report their PCI devices with vendor, device, class and NUMA locality, and a VM template asks for `PCI = [ CLASS = "0302", VENDOR = "10de" ]` (docs.opennebula.io/7.4, NVIDIA GPU passthrough). That is a request by kind, not by address.

## Decision

1. **A GPU Machine runs on mvmd's Cloud Hypervisor Driver with whole-device VFIO passthrough.** One device belongs to one Machine. A GPU host's mvmd serves the Cloud Hypervisor Driver. Firecracker stays the Driver of hosts without GPUs where its configuration names it.
2. **GPU hosts are bare metal.** A nested host (`naru.garam.sh/virtualization: nested`, [ADR 0110](./0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md) item 3) reports no GPU. Passing a device through a Proxmox VM into a nested microVM is not supported.
3. **The host prepares the devices at boot.**
   - **Configuration.** `spec.host.gpus` names the devices to pass, by PCI address or by `all` of the display classes `0300` and `0302`.
   - **Binding.** Init binds each named device to `vfio-pci` by `driver_override` before any other driver probes it. The host image carries no GPU driver.
   - **Kernel.** The host kernel boots with the IOMMU on (`intel_iommu=on iommu=pt`, or the AMD equivalent) and has VFIO built in.
   - **Hugepages.** `spec.host.hugepages` reserves 1 GiB pages per NUMA node at boot.
4. **The host reports its devices, and a device that cannot be isolated is not offered.**
   - **The report.** mvmd's runtime information reports each device: PCI address, vendor and device IDs, model name, NUMA node, IOMMU group, memory, and the Machine that holds it, if any. hostd passes it on in `GET /v1/host`.
   - **Isolation.** A device whose IOMMU group holds any other device except PCI bridges and the GPU's own functions (its audio function) is reported as not passable, with the reason.
   - **At admission.** The operator sees the devices of a pending host before admitting it, since `GET /v1/pending` carries the reported hardware ([orch.md](../orch.md) §Admitting a host that booted from an install seed).
5. **A use asks for a model and a count, and mvmd chooses the devices.** The shape's `gpu` is a `model` and a `count`.
   - **Orch** places by the free count of that model on each host ([ADR 0110](./0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md)).
   - **Create Machine** in mvmd takes `devices` as a class, model and count. mvmd chooses free devices from as few NUMA nodes as hold the count, and records each as held, as a disk has one holder. The host's addresses and topology stay on the host.
6. **A GPU Machine is pinned for performance.**
   - **vCPUs.** Its vCPUs are pinned one to one to physical cores of its devices' NUMA nodes, cores no other Machine is pinned to.
   - **Memory.** Its memory is backed by 1 GiB hugepages on those nodes, and the guest sees that NUMA layout.
   - **Feeding the devices.** Its spec's `cpu` must be at least 8 per GPU, so CPU-side preprocessing does not starve the devices, which is what the 37% case measured; a create below that is 422 naming the minimum. Its `memoryBytes` must be a whole number of GiB, since it is backed by 1 GiB pages (422 otherwise). The spec's figures are what placement counts ([ADR 0110](./0110-orch-places-a-machine-on-the-host-that-fits-its-shape-by-capacity-orch-accounts-for-itself-and-labels-and-spreads-machines-across-hosts-amending-adr-0042s-placement.md) item 1), so the account and the pinning agree.
   - **Placement.** A host is a candidate only with those cores and hugepages free on the devices' nodes.
7. **The capabilities say what a device costs, in the same API.** A Machine with a device reports `snapshot: [volumes]` (no `memory`), `move: [cold]` and no `resize`. A move stops it, places it and starts it on another host with the same model free ([ADR 0109](./0109-each-host-is-authoritative-for-what-runs-on-it-and-keeps-running-it-without-orch-orch-keeps-desired-state-placement-and-routes-and-a-machine-is-rebound-only-on-its-old-hosts-acknowledgement-or-abandonment.md) item 5). Every other call is the same as for a Machine without one.
8. **The guest has the GPU driver.** naru builds a guest kernel variant with the NVIDIA open GPU kernel modules built against it, named in `guest.json` like the other guest components ([ADR 0010](./0010-the-guest-kernel-and-initrds-are-image-components-built-at-the-pinned-mvmd-commit.md)). A GPU Machine boots that kernel. The CUDA user space is the Machine's image's.
9. **The GPU goal is done when a training benchmark is measured both ways.** A fixed training benchmark runs in a GPU Machine and on the same host booted bare metal with the same driver. Its throughput ratio is recorded on the goal's issue. The goal states the benchmark and the ratio it accepts, with the 13% average above as the reference.
10. **The mvmd work.** Each item is an mvmd issue of the later goal:
    - device attach in the runtime API (Create Machine's `devices`; Snapshot refused while a device is attached, as a precondition failure);
    - the Cloud Hypervisor Driver's `--device`, `--cpus affinity`, `--memory hugepages` and `--numa`, with a capability `device-passthrough` the Firecracker Driver does not report;
    - the device inventory in runtime information.

## Options considered

- **QEMU as a third mvmd Driver, as Ubicloud chose for B200.** QEMU builds a PCIe hierarchy per device, which CUDA on HGX needed. It is also a much larger VMM to confine than mvmd's two (mvmd ADR 0015's confinement is written for them), and the case that forced it (HGX with NVSwitch partitions) is not the consumer GPU of the input. Rejected for now. The Driver port admits a QEMU Driver later without a change to naru's API, and §Signs the decision was wrong names when.
- **vGPU or MIG to share a device.** Consumer GPUs offer neither. Rejected.
- **Orch names PCI addresses.** Orch would carry each host's topology and IOMMU groups. Choosing devices by NUMA is the host's knowledge, as choosing a disk's path is. Rejected.
- **GPU passthrough on nested hosts.** It needs the device passed into the Proxmox VM and again into the microVM, with nested IOMMU emulation. Rejected (input of #373).
- **Snapshot or live move of a GPU Machine.** A device's state is not in the VM's snapshot, and Cloud Hypervisor does not migrate a VFIO device. Rejected; the Machine reports `cold` only.

## Consequences

- GPU Machines need a bare-metal host joined to the fleet, which is the later goal's remote-host work (ADR 0113, the host mesh) unless the host is on the site's LAN.
- naru's image work: the host kernel's IOMMU and VFIO options, `spec.host.gpus` and `spec.host.hugepages` in init, and the guest kernel variant with the GPU modules.
- A host's pinned cores and hugepages are not available to other Machines, so a GPU host's CPU account (ADR 0110 item 1) subtracts them.

## Signs the decision was wrong

- CUDA fails to initialise in a Cloud Hypervisor guest for a GPU the fleet buys, as it did on B200. A QEMU Driver behind mvmd's port is then the answer.
- The measured ratio of item 9 is worse than the 13% reference by a margin the goal did not accept, with the vCPUs pinned and hugepages on.
- Uses want several GPUs per Machine with NVLink or NVSwitch, which needs Fabric Manager partitions as Ubicloud's B200 work did.
