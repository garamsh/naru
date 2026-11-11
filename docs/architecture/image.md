# Image

What the P0 image holds, where its inputs come from, and how a Node boots it.

Claims about garamsh/mvmd hold at commit `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64`; they promise nothing about later versions.

## Current decisions

- **P0 is done when a QEMU VM boots the image and init, as PID 1, starts and supervises the services the configuration names.** The services are mvmd, gaon's Node Agent and optionally gaon's control plane.
- **QEMU boots a kernel and an initramfs directly** (`-kernel`, `-initrd`). There is no installer and no disk layout for the system. The root is the read-only initramfs.
- **Two disks are attached.** The configuration disk is described in [config.md](./config.md). The data disk holds mvmd's state directory, formatted ext4 with the `verity` feature.
- **Host kernel.** kernel.org longterm 6.18.63, built by naru from the release tarball with naru's configuration, the version and the tarball's sha256 pinned in `image/kernel/`. It meets the conditions table of [ADR 0006](./adr/0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md).
- **Non-Go inputs** are `image/kernel/` (configuration, pin and the build script) and `image/initramfs/` (pin, the entry list and the build script); `image/build.env` fixes the environment both scripts build in.
- **Binaries in the initramfs:**
  - naru-init as `/init`.
  - mvmd and mvmd-supervisor, built with Go (`CGO_ENABLED=0`) from the mvmd commit pinned in `image/initramfs/pin.env`, `cfe57fb9f9bfbd7ec8e24b54915656030f1e2b64`, fetched with git into the build directory.
  - Firecracker, from the release that mvmd's `tooling/firecracker/fetch.sh` pins, verified by that pin. The script is run from the pinned checkout.
  - `naru-standin`, the stand-in service of the P0 test (see [Booting a Node](#booting-a-node)). gaon's binaries are built from a pinned gaon commit once gaon ships them, and arrive by a pin bump.
  - Nothing is built or run in a container. There is no shell and no busybox.
- **Not in the P0 image:** the guest kernel, and mvmd's guest-agent and converter initrds. They are needed only to run a Machine.
- **Forwarding is set by init**, not by mvmd ([config.md](./config.md)).

## Building the image

- `make image` writes `build/image/bzImage` and `build/image/initramfs.cpio.gz`, and for the tests of Machines `vmlinux` (the guest kernel) and `inputs.env` (mvmd's names for it and for Firecracker, with their digests). `build/` is not tracked.
- **The kernel.** `image/kernel/build.sh` downloads the pinned tarball, fails unless its SHA-256 is the pinned one, merges `image/kernel/naru.config` over the x86_64 `defconfig` with the kernel's `merge_config.sh`, runs `olddefconfig`, and then fails unless every line of `image/kernel/required.txt` matches a line of the final `.config`: kconfig drops an option whose dependencies are unmet without failing, so the file the build uses is what is checked. The unpacked source and the object directory are emptied before each build.
- **The initramfs.** `image/initramfs/build.sh` builds the programs, fails unless each is a static executable, and writes the archive with the kernel's own `usr/gen_init_cpio`, compiled from the pinned tarball. The archive is newc, in the order of `image/initramfs/initramfs.list`, every entry owned by 0:0 and stamped with `SOURCE_DATE_EPOCH` from `image/build.env`, then compressed with `gzip -9n`.
- **Reproducibility.** `make image-repro` builds twice from empty build directories and fails unless `bzImage` and `initramfs.cpio.gz` have equal SHA-256. It holds for one host toolchain: the kernel build records the compiler's version, so a host with another gcc or Go builds other bytes.
- **Who moves the pins.** The kernel's pin is moved by the PM ([image/kernel/pin.env](../../image/kernel/pin.env)), and so is mvmd's ([image/initramfs/pin.env](../../image/initramfs/pin.env)); both name how.

## Booting a Node

- `make test-qemu` builds the image, then boots one Node under QEMU and reads its serial console. QEMU runs as the user with `-enable-kvm -cpu host`, `-kernel`, `-initrd`, and two virtio disks; nothing needs root.
- **The disks** are built by `tooling/qemu/internal/node` with `/sbin/mkfs.ext4 -d`, which fills an ext4 image from a directory without mounting it. The configuration disk is labelled `NARUCFG` and holds `naru.yaml`. The data disk is labelled `NARUDATA`, has the `verity` feature, and holds `config/mvmd.json`; `naru.yaml` mounts it at `/var/lib/mvmd`, and the mvmd service's `-config` names the file there, so init knows nothing of mvmd's configuration.
- **The services** are `mvmd` and `standin`. The stand-in is `tooling/qemu/cmd/naru-standin`: it logs the file system type of the mount named by `-mount`, read from `/proc/self/mountinfo`, and dials the Unix socket named by `-socket` until it accepts a connection. It is a module of its own under `tooling/` because it and the QEMU test are test-only code and are not part of naru-init's module.
- **How the test knows mvmd serves.** mvmd logs nothing when it starts listening, so the stand-in, a check program in the image, logs `socket accepts connections` once a dial succeeds. The test reads that line.
- **What the test asserts**, each on the console: init's `configuration accepted`; `started` for `mvmd` and for `standin`; the stand-in's report that `/var/lib/mvmd` is mounted as ext4; and the stand-in's report that `/run/mvmd.sock` accepts connections. A service that does not start leaves its `started` line out, and the test fails.
- **mvmd's driver** in the P0 test configuration is `memory`: the Firecracker and Cloud Hypervisor drivers need the guest kernel and the guest-agent and converter initrds at start, which the P0 image does not carry. The `supervision` and `network` sections are present: mvmd reconciles its nftables tables when it starts with a `network` section, so the test exercises the kernel's nf_tables, and the supervision section names `/sys/fs/cgroup/naru/detached`. No test starts a supervisor yet.

## What the host needs

- Linux x86_64 with `/dev/kvm` readable and writable by the user. Part B's Nodes run Firecracker inside QEMU, which needs nested KVM (`/sys/module/kvm_intel/parameters/nested` or `kvm_amd`'s reads `Y` or `1`).
- To build: bash, curl, tar, xz, gzip, git, file, gcc, make, flex, bison, bc, perl, and the libelf and OpenSSL headers the kernel build asks for; zstd, which mvmd's fetch.sh unpacks the guest kernel with; and Go at the version mvmd's `go.work` names, which Go's toolchain download supplies.
- To boot: `qemu-system-x86_64` and `/sbin/mkfs.ext4` (e2fsprogs 1.43 or later for `-d`).
- Network access for the first build: cdn.kernel.org, github.com, and the Go module proxy.

## Rationale

- [ADR 0004](./adr/0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md): why P0 boots a kernel and initramfs directly and keeps the state directory on a data disk.
- [ADR 0006](./adr/0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md): why the kernel is a pinned longterm tarball, which condition comes from which mvmd document, and why the binaries are built from pins.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): the obligations the image meets; the table of them is in [boundaries.md](./boundaries.md).

## Open questions

- **Installer.** How an image reaches a Node's disk is not decided.
- **A/B upgrade.** How a Node upgrades and rolls back is not decided.
- **Signing.** How an image, a kernel and a configuration are signed and verified is not decided.
- **Disk layout.** Partitions of an installed Node, and where the system lives other than as an initramfs, are not decided.
- **The guest artifacts.** Which guest kernel and which guest-agent and converter initrds a Node carries, and who builds the Rust initrds, is not decided; P0 omits them.
- **How the other mvmd obligations are met.** The capabilities' grant and the nftables ownership are not decided; they wait under [boundaries.md](./boundaries.md) §Open questions. The cgroup placement of supervisors is decided: [init.md](./init.md) §What init requires of mvmd's placement.
