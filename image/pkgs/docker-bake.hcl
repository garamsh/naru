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
variable "OUT" {}
# The version naru-init reports and the naru-orch extension is built for (VERSION of the Makefile).
variable "NARU_VERSION" {}

group "default" {
  targets = ["builder", "toolchain", "util-linux", "e2fsprogs", "liburcu", "libinih", "xfsprogs", "libmnl", "libnftnl", "jansson", "nftables", "cacert",
    "naru", "extension", "etcd", "cni", "ecr-login", "gaon", "kubernetes",
    "libseccomp", "containerd", "rolesanywhere"]
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
