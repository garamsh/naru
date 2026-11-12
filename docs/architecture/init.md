# Init

What naru's init does as PID 1 of a Node: how it boots, starts, supervises and stops the services, and what it requires of them.

Claims about garamsh/mvmd hold at commit `1b08e5e04273b8839f66d9e154bf96086fbc3d23`; they promise nothing about later versions.

## Current decisions

- **Unit.** Init is the Go module `init/`, built from `init/cmd/naru-init` as a static binary (`CGO_ENABLED=0`), since the image has no libc (ADR 0004). It refuses to run unless it is PID 1, except as `naru-init -validate <file>`, which parses the file as `naru.yaml` with the parser init uses at boot, prints the Node's name and service count, and exits 0, or names the refusal and exits 1; the release's tests use it (it is the one way another module reaches init's parser, since `internal/` is closed to them).
- **Boot order.** Init does these in order, and stops at the first that fails:
  1. leaves the initramfs's rootfs: copies the initramfs to a tmpfs, moves that tmpfs over `/` and chroots into it (the kernel refuses `pivot_root` from the rootfs, which mvmd's supervisor does for each VMM);
  2. mounts `/proc`, `/sys`, `/dev` (devtmpfs), `/dev/pts`, `/run` (tmpfs), `/tmp` (tmpfs) and `/sys/fs/cgroup` (cgroup2), and makes Ctrl-Alt-Del deliver SIGINT to init;
  3. finds the block device whose ISO 9660 label is `cidata`, mounts it read-only, and parses `user-data` as `naru.yaml` and `network-config` ([config.md](./config.md));
  4. applies the network: sets `lo` up, then sets each link of the configuration up, adds its address and the default route through netlink, writes `/run/naru/resolv.conf`, and logs `network configured`;
  5. finds, checks and mounts the data disk;
  6. turns on forwarding for the families `spec.network.forwarding` names;
  7. writes the files `spec.files` lists;
  8. issues the certificates `spec.pki` lists, when it lists any, with the addresses of `network-config` as the Node's own;
  9. prepares the cgroup tree;
  10. remounts `/` read-only (`MS_REMOUNT|MS_RDONLY`), by which time every mount point exists, and logs `root remounted read-only`;
  11. starts each service in file order, leaving out one marked `controlPlane` on a Node whose `spec.controlPlane` is false.
- **The root is read-only when the first service starts.** The system is never written after init's seal; a service writes only to the mounts above (`/run`, `/tmp`, the data disk). A refusal to remount stops every service like any other refusal.
- **A refusal stops every service.** A configuration or network-config that does not parse or fails validation, a network that cannot be applied, a file that cannot be written, a certificate that cannot be issued, a data disk that is refused, a forwarding setting that cannot be written and a cgroup tree that cannot be built and a root that cannot be remounted read-only each stop the sequence before the first service starts. Init logs the reason, with the field named for a configuration error, to the console and stays up, so the reason stays readable and the Node can still be powered off by signal. It starts no service afterwards. Services that started before the refusal keep running and are supervised, and the certificates init issued for them are still renewed.
- **Disks are found by label.** Init reads each device in `/sys/class/block` itself and does not use `blkid`: the data disk by its ext4 superblock (magic `0xEF53` at offset 1080, label at 1144), the NoCloud drive by the volume label of its ISO 9660 primary descriptor (32 bytes at byte 40 of sector 16, `cidata` in either case). A disk that has not appeared when it is first looked for is waited for, up to 10 s.
- **The network is applied before any service.** A link is found by MAC address, else by name, and waited for up to 10 s. Init does not rename links and starts no DHCP client. The resolvers are in `/run/naru/resolv.conf` and `/etc/resolv.conf` is a link to it, so the read-only root holds what services read. `/run/naru` is init's: a `spec.files` path may not be under it.
- **`spec.files` are written before any service starts**, under `/run`, with the mode the file gives, creating parent directories and refusing an existing file.
- **`spec.pki` certificates are issued before any service starts, and renewed.** Init writes each authority's certificate, and its key only where the authority names a `keyPath`, then issues each certificate with a new ECDSA P-256 key made on the Node, writing the key mode `0600` and the certificate mode `0644` under `/run`, never to the data disk, and logs `authority written` and `certificate issued`. A certificate is valid for its `validity`, 720 h when it names none, and never past the authority's own expiry (init logs `authority expires soon` when the authority caps it); it is refused when that leaves under 1 h. The clock is the Node's wall clock at boot, which an emulated RTC gives a Node under Proxmox or QEMU. When two thirds of the life it was issued for has passed, init issues it again with a new key, writes the new key and certificate beside the old ones and renames them over, the key first, putting the old key back if the certificate's rename fails, and logs `certificate renewed`. After every renewal of a pass it restarts each service they name, once: stops it, kills what is left in its cgroup and starts it again with the same configuration. A service that is not running is not started by it. A renewal that fails is logged as `certificate renewal failed` and retried after a minute, and restarts nothing until it succeeds. A certificate that cannot be issued at boot stops the sequence like any other refusal, and when a later step refuses, init still renews what it issued, for the services that already started ([ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md)).
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
- **A service's environment is what the file gives it.** Init passes no variable of its own, runs the service in a new session, and gives it a pipe as standard output and error, which init copies to the console (see the next item).
- **The console log is kept and re-printed.** From before its first mount, init keeps every line it logs and every line its services write, and every 30 s writes all of them again in one block: `NARU-LOG-BEGIN seq=<n> lines=<k> dropped=<d>`, the lines, then `NARU-LOG-END seq=<n>`, with `seq` rising from 1. A reader that attaches late therefore has the whole log within 30 s. The kept lines are bounded at 1 MiB, and a line over 4096 bytes is kept as several; past the bound the oldest lines are dropped and counted in `dropped`, which is the number of lines dropped since boot. Kernel messages are not kept. A reader of the log treats each line as possibly repeated ([ADR 0009](./adr/0009-init-re-prints-the-console-log-at-a-fixed-interval-between-numbered-markers.md)).
- **Signals.** SIGTERM stops every service, syncs, unmounts the data disk and powers off. SIGINT does the same and reboots.

## Decided, not yet implemented

Decided in [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md) and [ADR 0018](./adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md) (garamsh/naru#46); garamsh/naru#47 builds them and moves each entry into §Current decisions. Until then init does none of it.

- **Boot order gains a choice.** After step 5 (the data disk mounts), init chooses the effective configuration: the stored one when its base digest equals the current `user-data`'s and it validates, otherwise the base, logging the discard and storing the base as revision 0. Steps 6 to 11 act on the effective configuration; steps 1 to 5 act on the base.
- **Init serves the Node API** when the effective configuration has `spec.api`: HTTPS, mutual TLS 1.3, `naru:admin` and `naru:reader` roles, and the five v1 endpoints. Without `spec.api` it logs that the API is off.
- **Init applies a configuration change on a running Node**: the store is written first; then per service it starts the added, stops the removed and restarts the changed, rewrites changed files and restarts the services their `restart` names, issues changed certificates again, and reloads the API's pair and `clientCA`. An unchanged service is not touched. A refusal changes nothing.
- **The kept log is attributed per service**, so a service's lines can be read back; the markers and the 30 s re-print of ADR 0009 are unchanged.

## What init requires of mvmd's placement

mvmd places its supervisors outside its own cgroup, so that stopping mvmd does not stop a running microVM (mvmd `runtime.md` §Fencing; mvmd ADR 0003 §Placement; mvmd ADR 0015). Read at the commit above, in `daemon/internal/supervision/supervision.go`, `supervision/cgroup.go` and `app/config.go`:

- `supervision.cgroup` is an absolute cgroup v2 directory in which mvmd starts each supervisor. mvmd opens it and does not create it, so it must exist when mvmd starts. If it is empty, the supervisors stay in mvmd's cgroup, which `cgroup.kill` on mvmd's leaf then kills with it.
- `supervision.vmm_cgroup` defaults to `/sys/fs/cgroup/mvmd-vmm`. mvmd creates it, writes `+cpu +memory +pids` to its `cgroup.subtree_control`, and makes one cgroup per VMM under it. That needs the three controllers enabled in the root's `subtree_control`.

What init provides, and what the Node's configuration must carry:

- Init creates `/sys/fs/cgroup/naru/detached` and enables the three controllers at the root and in `naru/`. mvmd's default VMM parent then works, and `detached` is outside mvmd's leaf `/sys/fs/cgroup/naru/mvmd`.
- The mvmd service's configuration must set `supervision.cgroup` to `/sys/fs/cgroup/naru/detached`. Init does not write mvmd's configuration (naru knows no service's internals, [config.md](./config.md)), so the Node's `naru.yaml` passes it in the service's `args` or `env`, or in a file of `spec.files` the service's `-config` names.
- mvmd's VMM cgroups go under `supervision.vmm_cgroup`, whose default is `/sys/fs/cgroup/mvmd-vmm` (`DefaultVMMCgroup` in `daemon/internal/supervision/supervision.go`, at the commit above). That is under the root, whose controllers init enables, so the Node's configuration leaves `supervision.vmm_cgroup` at its default.
- A host kernel built with `CONFIG_RT_GROUP_SCHED` needs the supervisors in the root cgroup (mvmd ADR 0015). Init does not provide that: it runs every service in `naru/`. Whether the P0 kernel is built with the option is [image.md](./image.md)'s to decide.

## Pinned dependencies

`init/go.mod` pins `go.yaml.in/yaml/v4`, `github.com/vishvananda/netlink` (the netlink client, with `github.com/vishvananda/netns` and `golang.org/x/sys` beneath it) and `github.com/stretchr/testify`; `go.sum` and the Go checksum database fix what each version means. The PM moves them: the PM reviews the pins when an advisory is published for one of them and at each Go minor release, and bumps the module in a pull request. No dependency bot is configured (`.github/` holds no `dependabot.yml`).

## Rationale

- [ADR 0018](./adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md): the store, the apply and the boot choice.
- [ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md): the API init serves and who may call it.
- [ADR 0013](./adr/0013-init-issues-certificates-at-boot-from-a-ca-carried-in-spec-pki-and-renews-them.md): why init issues the `spec.pki` certificates with keys made on the Node, the renewal at two thirds of the validity, and the signs it was wrong.
- [ADR 0004](./adr/0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md): why init is PID 1 of a read-only initramfs and supervises the configured services.
- [ADR 0003](./adr/0003-naru-deploys-and-supervises-mvmd-builds-both-kernels-and-carries-mvmds-deployment-constraints.md): the data disk's `verity` requirement, the forwarding obligation, and mvmd's supervisor placement.
- [ADR 0008](./adr/0008-a-node-reads-its-configuration-and-network-from-a-cloud-init-nocloud-drive.md): where init reads the configuration and network from, and why netlink.
- [ADR 0009](./adr/0009-init-re-prints-the-console-log-at-a-fixed-interval-between-numbered-markers.md): why init keeps the console log and re-prints it, and the marker format.
- [ADR 0005](./adr/0005-configuration-is-a-yaml-file-on-a-labelled-ext4-disk-in-the-shape-of-gaons-resources.md): the shape of the file init validates.
- [ADR 0001](./adr/0001-the-repository-is-a-go-workspace-of-executable-units-partitioned-by-responsibility.md): why init is a module of its own.

## Open questions

- **Verifying the P0 kernel's options.** Whether the image's kernel has `CONFIG_RT_GROUP_SCHED` off and cgroup v2 with the three controllers on is checked by the QEMU test of the image, not by init.
- **Boot and PID 1 behaviour.** Init's behaviour as PID 1 (mounts, console, power-off, reboot) is covered by the QEMU test of the image, not by `make test`.
- **A failed service.** A service that keeps failing is restarted at 30 s forever; whether init gives up is not decided. How a Node reports it is decided ahead: `GET /v1/services` gives each service's state and restart count ([ADR 0017](./adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md)).
