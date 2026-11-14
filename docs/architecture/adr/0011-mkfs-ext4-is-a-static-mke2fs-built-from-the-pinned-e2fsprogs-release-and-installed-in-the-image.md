# ADR 0011: mkfs.ext4 is a static mke2fs built from the pinned e2fsprogs release and installed in the image

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

Drafted for garamsh/naru#27, part of goal garamsh/naru#25. It answers the open question "mkfs.ext4 in the image" of [image.md](../image.md); it supersedes no ADR. The e2fsprogs facts below are read from the 1.47.4 release tarball and its configure script.

## Context

- **mvmd runs `mkfs.ext4` to format each Machine's disk,** at the path of its `disk.mkfs` setting, with the arguments `-q -F -U <uuid> <path>` (`exec.Command(mkfs, "-q", "-F", "-U", uuid, path)` in mvmd's `daemon/internal/diskfile/disk.go` line 332 at commit 3b37892f303a4d4dbd8e5360906d0f9c036b562e); it passes no `-t`. The image had none.
- **The QEMU tests worked around it.** They staged the host's dynamically linked `mke2fs`, its loader and its libraries on the data disk, behind a static Go launcher (`tooling/qemu/cmd/naru-mkfs`) that ran the loader. A Node booted from the image alone, on Proxmox or in gaon's end-to-end test, could not run a Machine that has a disk.
- **The image has no libc and no shell.** Every program in the initramfs is a static executable ([ADR 0004](./0004-p0-boots-a-host-kernel-and-a-read-only-initramfs-in-qemu-and-naru-init-supervises-the-configured-services.md), image.md §Binaries in the initramfs), so a dynamic mke2fs would need a loader and libraries beside it.
- **Inputs are pinned and checked.** The kernel is a tarball pinned by SHA-256 ([ADR 0006](./0006-the-host-kernel-is-built-from-the-pinned-longterm-6-18-tarball-and-the-image-inputs-live-in-image.md)); the same form fits a second source.
- **What mke2fs needs from the package.** `misc/mke2fs` links `libext2fs`, `libe2p`, `libcom_err`, `libsupport`, `libuuid` and `libblkid` (read from `misc/Makefile`'s `mke2fs` rule), all built in the same tree.

## Decision

**naru builds `mke2fs` from the pinned e2fsprogs 1.47.4 source tarball, statically linked with the host's compiler, and the initramfs installs it as `/bin/mkfs.ext4`; mvmd's `disk.mkfs` names that path.**

1. **The source is the release tarball from kernel.org,** `https://cdn.kernel.org/pub/linux/kernel/people/tytso/e2fsprogs/v1.47.4/e2fsprogs-1.47.4.tar.xz`, the newest stable release in that directory when this was decided. `image/e2fsprogs/pin.env` records its version, URL and SHA-256, `fd5bf388cbdbe006a3d3b318d983b2948382440acc85a87f1e7d108653e8db0b`. The digest was read from `sha256sums.asc` beside the release in that directory (PGP-signed by the project; the signature was not verified here, only the digest compared) and equals the digest of the downloaded file. `image/e2fsprogs/build.sh` fails unless the file it holds has that digest, before it unpacks anything.
2. **mke2fs only.** The build configures an out-of-tree build and runs `make libs` and `make -C misc mke2fs`, so no other tool of the package is built. mvmd calls `mkfs.ext4` and nothing else of e2fsprogs.
3. **Static with the host toolchain.** `LDFLAGS=-static`, so the host's `libc.a` is used (no musl and no download), and libuuid and libblkid are the tree's own (`--enable-libuuid --enable-libblkid`) so that configure does not find a system copy and link it. The build fails unless `file` reads the stripped binary as statically linked, as `image/initramfs/build.sh` does for every program. Nothing runs as root and no package is installed beyond the build dependencies the image already lists (a compiler, `libc.a`, `strip`, make).
4. **What is switched off, and why.** `--disable-nls` (mke2fs links no gettext in the image, and a message catalogue has no place in a read-only initramfs), `--disable-fuse2fs` (needs libfuse, and naru mounts nothing through FUSE), `--disable-debugfs`, `--disable-imager`, `--disable-resizer`, `--disable-defrag` (programs that mke2fs does not link and mvmd does not run), `--disable-uuidd` (a daemon), `--disable-e2initrd-helper`, `--disable-elf-shlibs` and `--disable-bsd-shlibs` (no shared libraries: mke2fs is static), `--disable-rpath` and `--disable-testio-debug`. e2scrub and the fsck wrapper are not built by the targets named in item 2, and the configure options that build them stay at their defaults.
5. **Reproducible.** `-ffile-prefix-map` maps the source and object directories, `-Wl,--build-id=none` drops the build id, `-g0` and `strip --strip-all` remove debug information, and `image/build.env` sets `SOURCE_DATE_EPOCH`. Two builds in different directories gave byte-identical files (same SHA-256), and `make image-repro` compares `mke2fs` of two clean builds.
6. **A stamp skips an unchanged build,** over `pin.env`, `build.sh` and `image/build.env`, as the kernel builds do.
7. **The path is `/bin/mkfs.ext4`,** beside `/bin/mvmd` and the other programs, so the list needs no new directory. mvmd passes `-q -F -U <uuid> <path>` and no `-t` (Context), so mke2fs takes the file system type only from the name it is run as, which is why the file is named `mkfs.ext4`. With no `/etc/mke2fs.conf` in the image mke2fs uses the defaults compiled into it. The QEMU Nodes' and the release's mvmd configuration name the path through `node.MkfsPath`; a test fails if the configuration names another path or the initramfs list does not install it.
8. **The data disk stops carrying `mke2fs`, the loader, the libraries and the launcher.** `tooling/qemu/cmd/naru-mkfs` and `tooling/qemu/internal/mkfs` are removed; nothing else used them. The host's `/sbin/mkfs.ext4` still makes the data disk itself (`-d`, without root), as before.

## Added size and memory

Measured at this commit, against the same tree without the `/bin/mkfs.ext4` entry:

| | Without the entry | With it | Added |
|---|---|---|---|
| `initramfs.cpio` (uncompressed) | 58,129,920 B | 59,514,880 B | 1,384,960 B (1.32 MiB) |
| `initramfs.cpio.gz` (what the ESP holds) | 20,411,586 B | 21,056,151 B | 644,565 B (0.61 MiB) |

"Without the entry" is the list with the one `mkfs.ext4` line removed, archived by the same `gen_init_cpio` and compressed with the same `gzip -9n`; it equals the "After" row of [ADR 0010](./0010-the-guest-kernel-and-initrds-are-image-components-built-at-the-pinned-mvmd-commit.md) byte for byte. The binary is 1,384,712 B stripped. init copies the extracted initramfs to a tmpfs and removes each entry from the rootfs as it is copied, so a Node holds the extracted size in memory for as long as it runs, about 1.32 MiB more; the figure is derived from the file size and init's code, not read from a running Node. The boot disk's ESP grows by the compressed 0.61 MiB.

## Consequences

- A Node booted from the image alone can format a Machine's disk. Nothing mvmd runs for it depends on the host, the data disk or a loader.
- The data disk holds only test inputs.
- The image is built from a second pinned tarball, so a new pin moves with a release of e2fsprogs; the PM reviews it when a release or an advisory is published.
- mke2fs's feature defaults are the ones of 1.47.4 (`metadata_csum`, `orphan_file`, `64bit` among them), not those of whatever the host had. A guest kernel that cannot mount a feature of the disk shows as a failed Machine boot; the guest kernel here has `EXT4_FS`.
- The build runs the package's own `configure`, which probes the host compiler; the bytes hold for one host toolchain, as the kernel's do.

## Signs the decision was wrong

- A Machine disk formatted by this mke2fs does not mount in a guest kernel, or mvmd's call to `mkfs.ext4` fails on an argument this binary does not take.
- A host without `libc.a` (or a runner image that drops it) makes the build fail, which would move the build to musl or a container.
- The memory of the binary in the root (§Added size and memory) is a problem on a small Node.
- A security advisory for e2fsprogs arrives faster than the pin is moved.
- mvmd starts to call another e2fsprogs tool, which would widen the build beyond mke2fs.

## Errata

- 2026-11-15: paths moved in the image split ([ADR 0054](./0054-the-image-is-split-into-image-host-and-image-node-with-what-both-carry-in-image-common-and-the-nodes-initramfs-has-its-own-list.md)): `image/e2fsprogs/` is `image/common/e2fsprogs/` and `image/initramfs/build.sh` is `image/host/initramfs/build.sh`.
