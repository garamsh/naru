# The environment every step of `make image` and `make release` runs in (docs/architecture/image.md
# §The builder, ADR 0063). Every input is a digest, a checksum or a version fixed by a dated archive
# snapshot, so two machines that build this file get the same compiler, libc and tools.
#
# Moved by: the PM, in a pull request that rebuilds the image and compares its SHA256SUMS with a
# second machine's, when an advisory is published for a package below or at each Go release.
# No bot proposes it.

# ubuntu:24.04, the index digest of its multi-platform manifest list.
FROM ubuntu@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5

# Ubuntu's archive snapshot of this instant: it keeps every version below installable. The base
# image has no CA bundle, so the first install skips the check of the server's certificate; apt
# still verifies the signed index and every package against the archive keyring.
ARG SNAPSHOT=20261113T000000Z
RUN rm -f /etc/apt/sources.list.d/ubuntu.sources /etc/apt/sources.list \
 && printf 'Types: deb\nURIs: https://snapshot.ubuntu.com/ubuntu/%s\nSuites: noble noble-updates noble-security\nComponents: main universe\nSigned-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg\nCheck-Valid-Until: no\n' "$SNAPSHOT" >/etc/apt/sources.list.d/snapshot.sources \
 && apt-get -o Acquire::https::Verify-Peer=false update \
 && apt-get -o Acquire::https::Verify-Peer=false install --yes --no-install-recommends ca-certificates=20260816ubuntu0.24.04.2 \
 && rm -rf /var/log/apt /var/log/dpkg.log /var/log/alternatives.log /var/cache/ldconfig/aux-cache

# The C toolchain and the tools the build scripts call. The list is what the scripts use and no
# more: bash, make and the compiler for every build; flex, bison, bc, perl, libelf and OpenSSL for
# the kernel; pkg-config for containerd's cgo; bzip2 for the tarballs that need it; dosfstools,
# gdisk and qemu-utils for the disk; squashfs-tools for the system extension; e2fsprogs for the
# data disk (mke2fs -d) and debugfs.
RUN apt-get update \
 && apt-get install --yes --no-install-recommends \
      bash=5.2.21-2ubuntu4 \
      bc=1.07.1-3ubuntu4 \
      binutils=2.42-4ubuntu2.10 \
      bison=2:3.8.2+dfsg-1ubuntu0.24.04.1 \
      build-essential=12.10ubuntu1 \
      bzip2=1.0.8-5.1ubuntu0.1 \
      cpio=2.15+dfsg-1ubuntu2.1 \
      curl=8.5.0-2ubuntu10.16 \
      dosfstools=4.2-1.1build1 \
      e2fsprogs=1.47.0-2.4~exp1ubuntu4.1 \
      file=1:5.45-3build1 \
      flex=2.6.4-8.2build1 \
      gcc=4:13.2.0-7ubuntu1 \
      gcc-13=13.3.0-6ubuntu2~24.04.1 \
      gdisk=1.0.10-1build1 \
      git=1:2.43.0-1ubuntu7.4 \
      gzip=1.12-1ubuntu3.2 \
      libc6-dev=2.39-0ubuntu8.12 \
      libelf-dev=0.190-1.1ubuntu0.1 \
      libssl-dev=3.0.13-0ubuntu3.16 \
      make=4.3-4.1build2 \
      perl=5.38.2-3.2ubuntu0.7 \
      pkg-config=1.8.1-2build1 \
      qemu-utils=1:8.2.2+ds-0ubuntu1.18 \
      squashfs-tools=1:4.6.1-1build1 \
      tar=1.35+dfsg-3ubuntu0.4 \
      xz-utils=5.6.1+really5.4.5-1ubuntu0.4 \
 && rm -rf /var/lib/apt/lists/* /var/log/apt /var/log/dpkg.log /var/log/alternatives.log /var/cache/ldconfig/aux-cache

# Go: the release go.work names in its toolchain line, from go.dev's published checksum.
# GOTOOLCHAIN names the same release, so a go.mod that asks for a newer one fails instead of
# fetching it; the scripts that build another project's source name that project's own release.
ARG GO_VERSION=1.26.11
ARG GO_SHA256=9849f4e04ee35303714e34fbc3ac8ab44ea3e448a95aa182f91e3b9b79c710ec
RUN curl --fail --silent --show-error --location --retry 4 --output /tmp/go.tar.gz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
 && echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum --check - \
 && tar -xzf /tmp/go.tar.gz -C /usr/local \
 && rm /tmp/go.tar.gz

# rustup-init, which image/host/guest/build.sh runs to install the Rust toolchain that mvmd's
# rust-toolchain.toml names, into the cache directory below.
ARG RUSTUP_VERSION=1.29.3
ARG RUSTUP_SHA256=b1c8cb14e30889e7e2999e6b69e62d9e9b9ffc0f4df8180564d306d9680e6c55
RUN curl --fail --silent --show-error --location --retry 4 --output /usr/local/bin/rustup-init "https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/x86_64-unknown-linux-gnu/rustup-init" \
 && echo "${RUSTUP_SHA256}  /usr/local/bin/rustup-init" | sha256sum --check - \
 && chmod 0755 /usr/local/bin/rustup-init

# The repository is mounted at /naru and the caches live under its build/ directory, so the
# downloads survive a run and no file is written outside it. HOME is set by image/builder/run.sh.
ENV PATH=/naru/build/cache/cargo/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    CARGO_HOME=/naru/build/cache/cargo \
    RUSTUP_HOME=/naru/build/cache/rustup \
    GOPATH=/naru/build/cache/go \
    GOCACHE=/naru/build/cache/gocache \
    GOTOOLCHAIN=go${GO_VERSION} \
    NARU_GO_VERSION=go${GO_VERSION}
