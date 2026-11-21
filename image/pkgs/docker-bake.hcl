# The package graph (docs/architecture/image.md §The package graph, ADR 0075): every package is one
# Dockerfile in its own directory, and a dependency is a named context, so BuildKit builds a package
# after the ones it names and shares their results. build.sh runs this file in the pinned BuildKit of
# image/builder/builder.env, with the working directory image/pkgs, and sets the two variables.
#
# Each package's last stage is FROM scratch and holds only the files the package installs, under
# /usr (the toolchain under /toolchain, cacert under /etc/ssl/certs). The output of each is the OCI
# tar OUT/<name>.tar, which tooling/disk's naru-pkg unpacks; timestamps are rewritten to
# SOURCE_DATE_EPOCH and layers are not compressed, so two builds of one commit give one digest.

variable "SOURCE_DATE_EPOCH" {}
# The kernel's build timestamp, from image/build.env with SOURCE_DATE_EPOCH.
variable "KBUILD_BUILD_TIMESTAMP" {}
variable "OUT" {}
# The version naru-init reports and the naru-orch extension is built for (VERSION of the Makefile).
variable "NARU_VERSION" {}
# The directory holding ca.pem, the throwaway root a test image trusts, or nothing (ADR 0026).
# build.sh sets it.
variable "TESTROOT" { default = "initramfs-node/no-testroot" }
# The linker flag that makes the refuse variant's naru-init look for a data disk whose label is
# absent, so a trial boot of it refuses (ADR 0025).
variable "REFUSE_LDFLAGS" { default = "-X github.com/garamsh/naru/lib/init/linux.dataDiskLabel=naru67-absent" }

# What make image-upgrade adds to make image: the kernels the upgrade tests put on a running Node,
# which differ from the image's in naru-init (ADR 0025), and the naru-orch extension built for the
# next and the refusing version, which the bundles of an upgrade carry (ADR 0084).
group "upgrade" {
  targets = ["bzImage-next", "bzImage-refuse", "extension-next", "extension-refuse"]
}

group "default" {
  targets = ["builder", "toolchain", "util-linux", "e2fsprogs", "liburcu", "libinih", "xfsprogs", "libmnl", "libnftnl", "jansson", "nftables", "cacert",
    "naru", "extension", "etcd", "cni", "ecr-login", "gaon", "kubernetes", "coredns", "volumes",
    "libseccomp", "containerd", "rolesanywhere",
    "mvmd", "guest-initrds",
    "kernel-host", "kernel-node", "kernel-guest-naru",
    "initramfs-node", "initramfs-host", "bzImage"]
}

target "_package" {
  args = { SOURCE_DATE_EPOCH = SOURCE_DATE_EPOCH }
  contexts = {
    builder = "target:builder"
    lib     = "."
  }
}

# The builder is the stage 0 of every package: the same Dockerfile run.sh builds, so build.sh can
# check its config digest against builder.env. Its output is discarded after that check.
target "builder" {
  context    = "../builder"
  dockerfile = "Dockerfile"
  args       = { SOURCE_DATE_EPOCH = SOURCE_DATE_EPOCH }
  output     = ["type=oci,dest=${OUT}/builder.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "toolchain" {
  inherits = ["_package"]
  context  = "toolchain"
  contexts = { kernel = "kernel" }
  output   = ["type=oci,dest=${OUT}/toolchain.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "_ctoolchain" {
  inherits = ["_package"]
  contexts = { toolchain = "target:toolchain" }
}

target "util-linux" {
  inherits = ["_ctoolchain"]
  context  = "util-linux"
  output   = ["type=oci,dest=${OUT}/util-linux.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "e2fsprogs" {
  inherits = ["_ctoolchain"]
  context  = "e2fsprogs"
  output   = ["type=oci,dest=${OUT}/e2fsprogs.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "liburcu" {
  inherits = ["_ctoolchain"]
  context  = "liburcu"
  output   = ["type=oci,dest=${OUT}/liburcu.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "libinih" {
  inherits = ["_ctoolchain"]
  context  = "libinih"
  output   = ["type=oci,dest=${OUT}/libinih.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "xfsprogs" {
  inherits = ["_ctoolchain"]
  context  = "xfsprogs"
  contexts = {
    util-linux = "target:util-linux"
    liburcu    = "target:liburcu"
    libinih    = "target:libinih"
  }
  output = ["type=oci,dest=${OUT}/xfsprogs.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "libmnl" {
  inherits = ["_ctoolchain"]
  context  = "libmnl"
  output   = ["type=oci,dest=${OUT}/libmnl.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "jansson" {
  inherits = ["_ctoolchain"]
  context  = "jansson"
  output   = ["type=oci,dest=${OUT}/jansson.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "libnftnl" {
  inherits = ["_ctoolchain"]
  context  = "libnftnl"
  contexts = { libmnl = "target:libmnl" }
  output   = ["type=oci,dest=${OUT}/libnftnl.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "nftables" {
  inherits = ["_ctoolchain"]
  context  = "nftables"
  contexts = {
    libmnl   = "target:libmnl"
    libnftnl = "target:libnftnl"
    jansson  = "target:jansson"
  }
  output = ["type=oci,dest=${OUT}/nftables.tar,rewrite-timestamp=true,compression=uncompressed"]
}

# cacert needs no compiler.
target "cacert" {
  inherits = ["_package"]
  context  = "cacert"
  output   = ["type=oci,dest=${OUT}/cacert.tar,rewrite-timestamp=true,compression=uncompressed"]
}

# The Go programs. A Go package is built by the builder's Go, which downloads the release a
# program's own go.mod names; nothing else of the toolchain is in it.
target "naru" {
  inherits = ["_package"]
  context  = "naru"
  contexts = { src = "../.." }
  output   = ["type=oci,dest=${OUT}/naru.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "extension" {
  inherits = ["_package"]
  context  = "extension"
  contexts = { naru = "target:naru" }
  args     = { NARU_VERSION = NARU_VERSION }
  output   = ["type=oci,dest=${OUT}/extension.tar,rewrite-timestamp=true,compression=uncompressed"]
}

# The extension of the next version: the programs of the image under the manifest that names
# ${NARU_VERSION}-next, the version bzImage-next's naru-init reports.
target "extension-next" {
  inherits = ["_package"]
  context  = "extension"
  contexts = { naru = "target:naru" }
  args     = { NARU_VERSION = "${NARU_VERSION}-next" }
  output   = ["type=oci,dest=${OUT}/extension-next.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "extension-refuse" {
  inherits = ["_package"]
  context  = "extension"
  contexts = { naru = "target:naru" }
  args     = { NARU_VERSION = "${NARU_VERSION}-refuse" }
  output   = ["type=oci,dest=${OUT}/extension-refuse.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "etcd" {
  inherits = ["_package"]
  context  = "etcd"
  output   = ["type=oci,dest=${OUT}/etcd.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "cni" {
  inherits = ["_package"]
  context  = "cni"
  output   = ["type=oci,dest=${OUT}/cni.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "ecr-login" {
  inherits = ["_package"]
  context  = "ecr-login"
  output   = ["type=oci,dest=${OUT}/ecr-login.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "gaon" {
  inherits = ["_package"]
  context  = "gaon"
  output   = ["type=oci,dest=${OUT}/gaon.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "kubernetes" {
  inherits = ["_package"]
  context  = "kubernetes"
  output   = ["type=oci,dest=${OUT}/kubernetes.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "coredns" {
  inherits = ["_package"]
  context  = "coredns"
  output   = ["type=oci,dest=${OUT}/coredns.tar,rewrite-timestamp=true,compression=uncompressed"]
}

# The volume add-on's extension (ADR 0083, 0084): the driver and its sidecars, an image archive and the
# manifests in a squashfs, with the description make image reads as it reads the naru-orch extension's.
target "volumes" {
  inherits = ["_package"]
  context  = "volumes"
  contexts = { src = "../.." }
  args     = { NARU_VERSION = NARU_VERSION }
  output   = ["type=oci,dest=${OUT}/volumes.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "libseccomp" {
  inherits = ["_ctoolchain"]
  context  = "libseccomp"
  output   = ["type=oci,dest=${OUT}/libseccomp.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "containerd" {
  inherits = ["_ctoolchain"]
  context  = "containerd"
  contexts = { libseccomp = "target:libseccomp" }
  output   = ["type=oci,dest=${OUT}/containerd.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "rolesanywhere" {
  inherits = ["_ctoolchain"]
  context  = "rolesanywhere"
  output   = ["type=oci,dest=${OUT}/rolesanywhere.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "mvmd" {
  inherits = ["_package"]
  context  = "mvmd"
  output   = ["type=oci,dest=${OUT}/mvmd.tar,rewrite-timestamp=true,compression=uncompressed"]
}

# The initrds are built from the mvmd commit that the mvmd package pins; the second context gives
# the Dockerfile that pin and no other file.
target "guest-initrds" {
  inherits = ["_package"]
  context  = "guest-initrds"
  contexts = { mvmd-pin = "mvmd" }
  output   = ["type=oci,dest=${OUT}/guest-initrds.tar,rewrite-timestamp=true,compression=uncompressed"]
}

# The kernels share one Dockerfile and, with it, the unpacked sources; _kernel names what differs.
# The Node's kernel is the Host's configuration plus its own, as the boot set the Host boots a Node
# from (ADR 0054). The image takes bzImage, whose embedded initramfs is the Host's, from the group
# that follows initramfs-host.
target "_kernel" {
  inherits   = ["_package"]
  context    = "kernel"
  dockerfile = "Dockerfile"
  args       = { SOURCE_DATE_EPOCH = SOURCE_DATE_EPOCH, KBUILD_BUILD_TIMESTAMP = KBUILD_BUILD_TIMESTAMP }
}

target "kernel-host" {
  inherits = ["_kernel"]
  target   = "bare"
  args     = { KERNEL = "host", BASE = "host", IMAGE_TARGET = "bzImage" }
  output   = ["type=oci,dest=${OUT}/kernel-host.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "kernel-node" {
  inherits = ["_kernel"]
  target   = "bare"
  args     = { KERNEL = "node", BASE = "host", IMAGE_TARGET = "vmlinux" }
  output   = ["type=oci,dest=${OUT}/kernel-node.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "kernel-guest-naru" {
  inherits = ["_kernel"]
  target   = "bare"
  args     = { KERNEL = "guest-naru", BASE = "tinyconfig", IMAGE_TARGET = "vmlinux" }
  output   = ["type=oci,dest=${OUT}/kernel-guest-naru.tar,rewrite-timestamp=true,compression=uncompressed"]
}

# The kernel's own initramfs archiver, a tool of the initramfs packages and no package of the image.
target "gen-init-cpio" {
  inherits = ["_kernel"]
  target   = "gen-init-cpio"
}

target "_initramfs" {
  inherits = ["_package"]
  contexts = {
    src            = "../.."
    gen-init-cpio  = "target:gen-init-cpio"
    testroot       = TESTROOT
    naru           = "target:naru"
    e2fsprogs      = "target:e2fsprogs"
    xfsprogs       = "target:xfsprogs"
    nftables       = "target:nftables"
    util-linux     = "target:util-linux"
    etcd           = "target:etcd"
    ecr-login      = "target:ecr-login"
    rolesanywhere  = "target:rolesanywhere"
    kubernetes     = "target:kubernetes"
    containerd     = "target:containerd"
    cni            = "target:cni"
    coredns        = "target:coredns"
    cacert         = "target:cacert"
  }
}

target "_initramfs-node" {
  inherits = ["_initramfs"]
  context  = "initramfs-node"
}

target "_initramfs-host" {
  inherits = ["_initramfs"]
  context  = "initramfs-host"
  contexts = {
    mvmd              = "target:mvmd"
    gaon              = "target:gaon"
    kernel-node       = "target:kernel-node"
    kernel-guest-naru = "target:kernel-guest-naru"
    guest-initrds     = "target:guest-initrds"
  }
}

target "initramfs-node" {
  inherits = ["_initramfs-node"]
  args     = { NARU_VERSION = NARU_VERSION }
  output   = ["type=oci,dest=${OUT}/initramfs-node.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "initramfs-node-next" {
  inherits = ["_initramfs-node"]
  args     = { NARU_VERSION = "${NARU_VERSION}-next" }
  output   = ["type=oci,dest=${OUT}/initramfs-node-next.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "initramfs-node-refuse" {
  inherits = ["_initramfs-node"]
  args     = { NARU_VERSION = "${NARU_VERSION}-refuse", EXTRA_LDFLAGS = REFUSE_LDFLAGS }
  output   = ["type=oci,dest=${OUT}/initramfs-node-refuse.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "initramfs-host" {
  inherits = ["_initramfs-host"]
  contexts = { initramfs-node = "target:initramfs-node" }
  args     = { NARU_VERSION = NARU_VERSION }
  output   = ["type=oci,dest=${OUT}/initramfs-host.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "initramfs-host-next" {
  inherits = ["_initramfs-host"]
  contexts = { initramfs-node = "target:initramfs-node-next" }
  args     = { NARU_VERSION = "${NARU_VERSION}-next" }
  output   = ["type=oci,dest=${OUT}/initramfs-host-next.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "initramfs-host-refuse" {
  inherits = ["_initramfs-host"]
  contexts = { initramfs-node = "target:initramfs-node-refuse" }
  args     = { NARU_VERSION = "${NARU_VERSION}-refuse", EXTRA_LDFLAGS = REFUSE_LDFLAGS }
  output   = ["type=oci,dest=${OUT}/initramfs-host-refuse.tar,rewrite-timestamp=true,compression=uncompressed"]
}

# The Host's kernel with the Host's initramfs inside it (ADR 0020), the bzImage the image boots. The
# arguments are kernel-host's, so the kernel is built once and only the embedding is built again.
target "_bzimage" {
  inherits = ["_kernel"]
  target   = "bzimage"
  args     = { KERNEL = "host", BASE = "host", IMAGE_TARGET = "bzImage" }
}

target "bzImage" {
  inherits = ["_bzimage"]
  contexts = { initramfs = "target:initramfs-host" }
  output   = ["type=oci,dest=${OUT}/bzImage.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "bzImage-next" {
  inherits = ["_bzimage"]
  contexts = { initramfs = "target:initramfs-host-next" }
  output   = ["type=oci,dest=${OUT}/bzImage-next.tar,rewrite-timestamp=true,compression=uncompressed"]
}

target "bzImage-refuse" {
  inherits = ["_bzimage"]
  contexts = { initramfs = "target:initramfs-host-refuse" }
  output   = ["type=oci,dest=${OUT}/bzImage-refuse.tar,rewrite-timestamp=true,compression=uncompressed"]
}
