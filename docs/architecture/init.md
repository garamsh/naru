# Init

What naru's init does as PID 1 of a Node: how it boots, starts, supervises and stops the services, and what it requires of them.

Claims about garamsh/mvmd hold at commit `1b08e5e04273b8839f66d9e154bf96086fbc3d23`; they promise nothing about later versions.

## Current decisions

- **Unit.** Init is the Go module `init/`, built from `init/cmd/naru-init` as a static binary (`CGO_ENABLED=0`), since the image has no libc (ADR 0004). It refuses to run unless it is PID 1.
- **Boot order.** Init does these in order, and stops at the first that fails:
  1. mounts `/proc`, `/sys`, `/dev` (devtmpfs), `/dev/pts`, `/run` (tmpfs), `/tmp` (tmpfs) and `/sys/fs/cgroup` (cgroup2), and makes Ctrl-Alt-Del deliver SIGINT to init;
  2. finds the disk labelled `NARUCFG`, mounts it read-only and parses `naru.yaml` ([config.md](./config.md));
  3. finds, checks and mounts the data disk;
  4. turns on forwarding for the families `spec.network.forwarding` names;
  5. prepares the cgroup tree;
  6. starts each service in file order, leaving out one marked `controlPlane` on a Node whose `spec.controlPlane` is false.
- **A refusal stops every service.** A configuration that does not parse or fails validation, a data disk that is refused, a forwarding setting that cannot be written and a cgroup tree that cannot be built each stop the sequence before the first service starts. Init logs the reason, with the field named for a configuration error, to the console and stays up, so the reason stays readable and the Node can still be powered off by signal. It starts no service afterwards.
- **Disks are found by ext4 label.** Init reads the superblock of each device in `/sys/class/block` itself (magic `0xEF53` at offset 1080, label at 1144) and does not use `blkid`. A disk that has not appeared when it is first looked for is waited for, up to 10 s.
- **The data disk is a precondition of every service.** Init mounts it with `nosuid,nodev` only if its superblock is ext4 and has the `verity` feature (`ro_compat` bit `0x8000`). Otherwise it refuses, says which check failed, and starts no service. The check is made on every boot, before the mount, so mvmd is never started on a disk without `verity`, which mvmd would refuse to start on (mvmd ADR 0008, as [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md) records it).
- **Every service runs in a cgroup of its own.** The hierarchy under the cgroup v2 root is:

  ```
  /sys/fs/cgroup                 cpu, memory and pids enabled for children
  /sys/fs/cgroup/naru            the same
  /sys/fs/cgroup/naru/<service>  one leaf per service
  /sys/fs/cgroup/naru/detached   reserved, empty; processes a service places outside its own
  ```

  A service is created in its leaf (`CLONE_INTO_CGROUP`), so it is never outside it. `detached` is a reserved service name.
- **Stop touches only the leaf.** Init sends the service SIGTERM, gives it 5 s, then writes `cgroup.kill` in its leaf, waits for `cgroup.procs` to empty and removes the leaf. After every exit, init kills what the exit left in the leaf, so a restart starts from an empty cgroup. A process the service moved to another cgroup is in none of these and survives its stop and restart. The real-cgroup test of that is `make test-cgroup`.
- **Restart backoff.** A service that exits, or that cannot be started, is started again after 1 s, doubling per consecutive exit up to 30 s. A run of 60 s or longer resets the delay to 1 s. A stop ends the wait.
- **Forwarding.** Init writes `1` to `/proc/sys/net/ipv4/ip_forward` and `/proc/sys/net/ipv6/conf/all/forwarding` for the families named, before any service starts ([config.md](./config.md)).
- **Reaping.** One reaper owns `wait4(-1)` on SIGCHLD and reports the exit of each service it started, so a service's exit is never lost to an orphan's reaping. Every other child, orphans reparented to init included, is collected.
- **A service's environment is what the file gives it.** Init passes no variable of its own, runs the service in a new session, and gives it the console as standard output and error.
- **Signals.** SIGTERM stops every service, syncs, unmounts the data disk and powers off. SIGINT does the same and reboots.

## What init requires of mvmd's placement

mvmd places its supervisors outside its own cgroup, so that stopping mvmd does not stop a running microVM (mvmd `runtime.md` §Fencing; mvmd ADR 0003 §Placement; mvmd ADR 0015). Read at the commit above, in `daemon/internal/supervision/supervision.go`, `supervision/cgroup.go` and `app/config.go`:

- `supervision.cgroup` is an absolute cgroup v2 directory in which mvmd starts each supervisor. mvmd opens it and does not create it, so it must exist when mvmd starts. If it is empty, the supervisors stay in mvmd's cgroup, which `cgroup.kill` on mvmd's leaf then kills with it.
- `supervision.vmm_cgroup` defaults to `/sys/fs/cgroup/mvmd-vmm`. mvmd creates it, writes `+cpu +memory +pids` to its `cgroup.subtree_control`, and makes one cgroup per VMM under it. That needs the three controllers enabled in the root's `subtree_control`.

What init provides, and what the Node's configuration must carry:

- Init creates `/sys/fs/cgroup/naru/detached` and enables the three controllers at the root and in `naru/`. mvmd's default VMM parent then works, and `detached` is outside mvmd's leaf `/sys/fs/cgroup/naru/mvmd`.
- The mvmd service's configuration must set `supervision.cgroup` to `/sys/fs/cgroup/naru/detached`. Init does not write mvmd's configuration (naru knows no service's internals, [config.md](./config.md)), so the Node's `naru.yaml` passes it in the service's `args` or `env`.
- mvmd's VMM cgroups go under `supervision.vmm_cgroup`, whose default is `/sys/fs/cgroup/mvmd-vmm` (`DefaultVMMCgroup` in `daemon/internal/supervision/supervision.go`, at the commit above). That is under the root, whose controllers init enables, so the Node's configuration leaves `supervision.vmm_cgroup` at its default.
- A host kernel built with `CONFIG_RT_GROUP_SCHED` needs the supervisors in the root cgroup (mvmd ADR 0015). Init does not provide that: it runs every service in `naru/`. Whether the P0 kernel is built with the option is [image.md](./image.md)'s to decide.

## Pinned dependencies

`init/go.mod` pins `go.yaml.in/yaml/v4` and `github.com/stretchr/testify`; `go.sum` and the Go checksum database fix what each version means. The PM moves them: the PM reviews the pins when an advisory is published for one of them and at each Go minor release, and bumps the module in a pull request. No dependency bot is configured (`.github/` holds no `dependabot.yml`).

## Rationale

- [ADR 0004](./adr/0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md): why init is PID 1 of a read-only initramfs and supervises the configured services.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): the data disk's `verity` requirement, the forwarding obligation, and mvmd's supervisor placement.
- [ADR 0005](./adr/0005-configuration-is-a-yaml-file-on-a-labelled-ext4-disk-in-the-shape-of-gaons-resources.md): the file init reads and validates.
- [ADR 0001](./adr/0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md): why init is a module of its own.

## Open questions

- **Verifying the P0 kernel's options.** Whether the image's kernel has `CONFIG_RT_GROUP_SCHED` off and cgroup v2 with the three controllers on is checked by the QEMU test of the image, not by init.
- **Boot and PID 1 behaviour.** Init's behaviour as PID 1 (mounts, console, power-off, reboot) is covered by the QEMU test of the image, not by `make test`.
- **A failed service.** A service that keeps failing is restarted at 30 s forever; whether init gives up, and how a Node reports it, waits for a node API.
