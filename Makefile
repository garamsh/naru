.PHONY: lint format format-apply test test-cgroup test-qemu test-harness nodes judge-serial release build image image-upgrade disk image-repro ci

# The modules every check covers: the ones go.work lists, so a new module
# joins by adding its use line there.
MODULES := $(shell go list -m -f '{{.Dir}}')

# Where build puts the binaries.
BIN := $(CURDIR)/bin

# Where image keeps its downloads, sources and outputs; git ignores it.
BUILD := $(CURDIR)/build
IMAGE := $(BUILD)/image

define each_module
@set -e; \
n=0; \
for dir in $(MODULES); do n=$$((n+1)); $(1); done; \
if [ "$$n" -eq 0 ]; then echo "$@: go.work lists no module" >&2; exit 1; fi; \
echo "$@: covered $$n module(s) selected by go list -m over go.work"
endef

# count_packages prints the packages ./... selects in $$dir and fails on zero.
define count_packages
p=$$(cd "$$dir" && go list ./... | wc -l); \
echo "$@: $$dir: ./... selects $$p package(s)"; \
if [ "$$p" -eq 0 ]; then echo "$@: $$dir: no packages" >&2; exit 1; fi
endef

# No linter is configured yet, so lint is go vet. The second pass vets the
# files only the QEMU test builds.
lint:
	$(call each_module, $(count_packages); (cd "$$dir" && go vet ./... && go vet -tags qemu_real ./...))

format:
	$(call each_module, \
	files=$$(cd "$$dir" && git ls-files --cached --others --exclude-standard -z -- '*.go' | tr -cd '\0' | wc -c); \
	echo "$@: $$dir: $$files .go file(s) selected by git ls-files --cached --others --exclude-standard"; \
	if [ "$$files" -eq 0 ]; then echo "$@: $$dir: no .go files" >&2; exit 1; fi; \
	bad=$$(cd "$$dir" && git ls-files --cached --others --exclude-standard -z -- '*.go' | xargs -0 gofmt -l); \
	if [ -n "$$bad" ]; then echo "$$bad" >&2; echo "$@: $$dir: unformatted files above" >&2; exit 1; fi)

format-apply:
	$(call each_module, (cd "$$dir" && git ls-files --cached --others --exclude-standard -z -- '*.go' | xargs -0 gofmt -w))

# The tests need neither root nor a VM. The ones that need a delegated cgroup
# are skipped here and run by test-cgroup; those that need PID 1 run in the
# QEMU test of the image.
test:
	$(call each_module, $(count_packages); (cd "$$dir" && go test -race -count=1 ./...))

# test-cgroup runs the tests that use real cgroups, in a cgroup the user
# delegates to itself with systemd-run --user. They are not part of ci: a
# runner without a user systemd would fail them. It fails, and never skips,
# when the test did not run.
test-cgroup:
	@set -e; \
	bin=$$(mktemp -d); trap 'rm -rf "$$bin"' EXIT; \
	(cd init && go test -c -o "$$bin/linux.test" ./internal/linux); \
	log=$$(mktemp); trap 'rm -rf "$$bin" "$$log"' EXIT; \
	if ! systemd-run --user --scope --quiet -p Delegate=yes sh -c \
	  'NARU_TEST_CGROUP_ROOT=/sys/fs/cgroup$$(cut -d: -f3 /proc/self/cgroup) exec "$$0" -test.v -test.run "^TestCgroups_" -test.count=1' \
	  "$$bin/linux.test" >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	n=$$(grep -c '^--- PASS: TestCgroups_' "$$log" || true); \
	echo "$@: $$n test(s) passed, selected by -test.run '^TestCgroups_' in a delegated cgroup"; \
	if [ "$$n" -eq 0 ]; then echo "$@: no test ran" >&2; exit 1; fi

# naru-init is static: it runs in an initramfs with no libc. naructl is static too: it is a release
# asset an operator runs on any Linux amd64 host.
build:
	$(call each_module, $(count_packages); (cd "$$dir" && go build ./...))
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/naru-init ./init/cmd/naru-init
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o $(BIN)/naructl ./naructl/cmd/naructl
	@echo "build: $(BIN)/naru-init: $$(file -b $(BIN)/naru-init | cut -d, -f1-4)"
	@echo "build: $(BIN)/naructl: $$(file -b $(BIN)/naructl | cut -d, -f1-4)"

# image builds the host kernel, the guest kernels and initrds, the static mke2fs and mkfs.xfs, gaon's four
# services, etcd, and the initramfs that carries them, from pinned sources, into $(IMAGE): bzImage
# (the kernel with the initramfs inside it, ADR 0020; bzImage.bare is the kernel before that),
# initramfs.cpio.gz, inputs.env (the Firecracker pin the tests read) and guest/,
# which holds what the initramfs carries of the guests, with guest.json naming
# it. The scripts skip what their inputs say is built, and verify every download.
GUEST := $(IMAGE)/guest
E2FS := $(IMAGE)/e2fsprogs
GAON := $(IMAGE)/gaon
ETCD := $(IMAGE)/etcd
# REGTOOLS holds the credential helpers and the root bundle (ADR 0026). TEST_ROOT, empty for the
# release, is the PEM file of a throwaway root appended to the bundle: the test image's, which
# test-qemu builds and no release carries.
REGTOOLS := $(IMAGE)/regtools
TEST_ROOT ?=
XFS := $(IMAGE)/xfsprogs
# VERSION is the version naru-init reports on the Node API and the release assets carry: `git describe
# --tags --always`, or VERSION=.
VERSION ?= $(shell git describe --tags --always)
export NARU_VERSION := $(VERSION)
image:
	image/kernel/build.sh $(BUILD)/kernel $(IMAGE)
	image/e2fsprogs/build.sh $(BUILD)/e2fsprogs $(E2FS)
	image/xfsprogs/build.sh $(BUILD)/xfsprogs $(XFS)
	image/gaon/build.sh $(BUILD)/gaon $(GAON)
	image/etcd/build.sh $(BUILD)/etcd $(ETCD)
	image/ecr-login/build.sh $(BUILD)/ecr-login $(REGTOOLS)
	image/rolesanywhere/build.sh $(BUILD)/rolesanywhere $(REGTOOLS)
	image/cacert/build.sh $(BUILD)/cacert $(REGTOOLS) $(TEST_ROOT)
	image/mvmd-checkout.sh $(BUILD)/initramfs/mvmd
	image/guest/build.sh $(BUILD)/initramfs/mvmd $(BUILD)/kernel $(BUILD)/guest $(GUEST)
	image/initramfs/build.sh $(BUILD)/initramfs $(BUILD)/kernel $(IMAGE) $(GUEST) $(E2FS) $(GAON) $(ETCD) $(REGTOOLS) $(XFS)
	image/kernel/embed.sh $(BUILD)/kernel $(IMAGE) $(IMAGE)/initramfs.cpio.gz

# image-upgrade builds the two kernels the upgrade tests (ADR 0025) put on a running Node, from the
# programs and the kernel image built: next/bzImage is the same tree with the version $(VERSION)-next
# stamped in, and refuse/bzImage a naru-init that cannot find its configuration drive, so a trial
# boot of it refuses and rolls back by itself. The second kernel's version carries an extra linker
# flag, which build.sh passes to the linker as part of -X main.buildVersion=.
UPGRADE := $(BUILD)/upgrade
REFUSE_FLAG := -X github.com/garamsh/naru/init/internal/linux.seedLabel=naru67-absent
image-upgrade: image
	@set -e; \
	for v in next refuse; do \
	  rm -rf $(UPGRADE)/$$v; mkdir -p $(UPGRADE)/$$v; \
	  cp $(IMAGE)/bzImage.bare $(IMAGE)/kernel.stamp $(UPGRADE)/$$v/; \
	done; \
	NARU_VERSION='$(VERSION)-next' image/initramfs/build.sh $(BUILD)/initramfs $(BUILD)/kernel $(UPGRADE)/next $(GUEST) $(E2FS) $(GAON) $(ETCD) $(REGTOOLS) $(XFS); \
	image/kernel/embed.sh $(BUILD)/kernel $(UPGRADE)/next $(UPGRADE)/next/initramfs.cpio.gz; \
	NARU_VERSION='$(VERSION)-refuse $(REFUSE_FLAG)' image/initramfs/build.sh $(BUILD)/initramfs $(BUILD)/kernel $(UPGRADE)/refuse $(GUEST) $(E2FS) $(GAON) $(ETCD) $(REGTOOLS) $(XFS); \
	image/kernel/embed.sh $(BUILD)/kernel $(UPGRADE)/refuse $(UPGRADE)/refuse/initramfs.cpio.gz

# disk builds the UEFI disk image from image's outputs: naru.img, a raw GPT
# disk whose ESP holds the kernel as \EFI\BOOT\BOOTX64.EFI and has room for a second one, and
# naru.qcow2, the same disk for Proxmox's import_from. It needs mkfs.fat,
# sgdisk and qemu-img, and neither root nor mtools.
disk: image
	image/disk/build.sh $(BUILD)/disk $(IMAGE)

# image-repro builds the image and the disk twice from clean build directories
# and fails unless the kernel, the initramfs, the guest manifest (which names
# the SHA-256 of every guest file), mke2fs, mkfs.xfs, naru.img and naru.qcow2 of both are
# byte-identical. It takes two full kernel builds, two guest kernel builds, two
# builds of the initrds and two of mke2fs and mkfs.xfs.
image-repro:
	@set -e; \
	for i in 1 2; do \
	  rm -rf $(BUILD)/repro/$$i; \
	  image/kernel/build.sh $(BUILD)/repro/$$i/kernel $(BUILD)/repro/$$i/image >/dev/null; \
	  image/e2fsprogs/build.sh $(BUILD)/repro/$$i/e2fsprogs $(BUILD)/repro/$$i/image/e2fsprogs >/dev/null; \
	  image/xfsprogs/build.sh $(BUILD)/repro/$$i/xfsprogs $(BUILD)/repro/$$i/image/xfsprogs >/dev/null; \
	  image/gaon/build.sh $(BUILD)/repro/$$i/gaon $(BUILD)/repro/$$i/image/gaon >/dev/null; \
	  image/etcd/build.sh $(BUILD)/repro/$$i/etcd $(BUILD)/repro/$$i/image/etcd >/dev/null; \
	  image/ecr-login/build.sh $(BUILD)/repro/$$i/ecr-login $(BUILD)/repro/$$i/image/regtools >/dev/null; \
	  image/rolesanywhere/build.sh $(BUILD)/repro/$$i/rolesanywhere $(BUILD)/repro/$$i/image/regtools >/dev/null; \
	  image/cacert/build.sh $(BUILD)/repro/$$i/cacert $(BUILD)/repro/$$i/image/regtools >/dev/null; \
	  image/mvmd-checkout.sh $(BUILD)/repro/$$i/initramfs/mvmd; \
	  image/guest/build.sh $(BUILD)/repro/$$i/initramfs/mvmd $(BUILD)/repro/$$i/kernel $(BUILD)/repro/$$i/guest $(BUILD)/repro/$$i/image/guest >/dev/null; \
	  image/initramfs/build.sh $(BUILD)/repro/$$i/initramfs $(BUILD)/repro/$$i/kernel $(BUILD)/repro/$$i/image $(BUILD)/repro/$$i/image/guest $(BUILD)/repro/$$i/image/e2fsprogs $(BUILD)/repro/$$i/image/gaon $(BUILD)/repro/$$i/image/etcd $(BUILD)/repro/$$i/image/regtools $(BUILD)/repro/$$i/image/xfsprogs >/dev/null; \
	  image/kernel/embed.sh $(BUILD)/repro/$$i/kernel $(BUILD)/repro/$$i/image $(BUILD)/repro/$$i/image/initramfs.cpio.gz >/dev/null; \
	  image/disk/build.sh $(BUILD)/repro/$$i/disk $(BUILD)/repro/$$i/image >/dev/null; \
	done; \
	for f in bzImage e2fsprogs/mke2fs xfsprogs/mkfs.xfs etcd/etcd regtools/docker-credential-ecr-login regtools/aws_signing_helper regtools/ca-certificates.crt gaon/gaon-node-agent gaon/gaon-apiserver gaon/gaon-scheduler gaon/gaon-controllers initramfs.cpio.gz guest/guest.json naru.img naru.qcow2; do \
	  a=$$(sha256sum $(BUILD)/repro/1/image/$$f | cut -d' ' -f1); b=$$(sha256sum $(BUILD)/repro/2/image/$$f | cut -d' ' -f1); \
	  echo "$@: $$f build 1 $$a"; echo "$@: $$f build 2 $$b"; \
	  if [ "$$a" != "$$b" ]; then echo "$@: $$f differs between the builds" >&2; exit 1; fi; \
	done; \
	echo "$@: bzImage, mke2fs, mkfs.xfs, etcd, the two credential helpers, the root bundle, the four gaon binaries, initramfs.cpio.gz, guest.json, naru.img and naru.qcow2 are byte-identical across two clean builds"

# nodes boots N (default 2) Nodes of the image under QEMU on one network, as
# the user, and keeps them up until interrupted. Their disks and consoles are
# under $(BUILD)/nodes.
N ?= 2
nodes: disk
	rm -rf $(BUILD)/nodes
	cd tooling/qemu && go run ./cmd/naru-nodes -n $(N) -root $(CURDIR) -image $(IMAGE) -guest $(GUEST) -dir $(BUILD)/nodes

# judge-serial reads the serial logs of two Nodes, node-1 first, and exits 0 only when they show
# what the QEMU tests assert (tooling/qemu/internal/judge). The logs are raw captures: Proxmox's
# termproxy or `qm terminal`, or the console.log of a `make nodes` run started with -probe. With
# JUDGE_GAON=1 they are the logs of Nodes started from the release's naru-node-<n>.test.yaml,
# which run gaon's services in place of the stand-ins and the probe. With JUDGE_GAON=1, JUDGE_PKI=1
# also requires node-1's log to show init issuing its pairs from spec.pki, and
# JUDGE_NODE1_INTERFACES=<n> sets the interfaces node-1's network-config gives it (1 when unset;
# the QEMU tests' control-plane Node has 2). JUDGE_GAON=1 also needs JUDGE_NARUCTL=<a naructl
# binary, the release's naructl-<version>-linux-amd64>: the services to expect are the ones its
# `topology` command prints (ADR 0019). JUDGE_NODE_API=listening|off also requires init's Node API
# line on both Nodes, that it serves the API or that it says the API is off (the release's test
# user-data has no spec.api, so off); left out, the line is not judged.
judge-serial:
	@test -n "$(NODE1)" -a -n "$(NODE2)" || { echo "judge-serial: usage: make judge-serial NODE1=<log> NODE2=<log>" >&2; exit 2; }
	cd tooling/qemu && go run ./cmd/naru-judge $(if $(JUDGE_GAON),-gaon) $(if $(JUDGE_NARUCTL),-naructl "$(abspath $(JUDGE_NARUCTL))") $(if $(JUDGE_PKI),-pki) $(if $(JUDGE_NODE_API),-node-api $(JUDGE_NODE_API)) $(if $(JUDGE_NODE1_INTERFACES),-node1-interfaces $(JUDGE_NODE1_INTERFACES)) -node1 "$(abspath $(NODE1))" -node2 "$(abspath $(NODE2))"

# release writes the assets of a deployment into $(RELEASE): naru-<version>.qcow2 (the boot disk),
# naru-upgrade-<version>.tar (the upgrade bundle: the kernel and a manifest of its digest),
# naru-data-<version>.qcow2 (the one data disk both Nodes attach), naru-guest-<version>.json,
# naru-inputs-<version>.json (what naru-cluster reads besides the disks), naructl-<version>-linux-amd64
# (the operator's tool, which also writes the test user-data below from a CA that lives only for the
# run), naru-node-1.test.yaml and naru-node-2.test.yaml (test-only user-data for Proxmox: they carry
# gaon's keys, so they are for a throwaway deployment) and SHA256SUMS. VERSION is defined above image.
RELEASE := $(BUILD)/release
release: disk
	rm -rf $(RELEASE)
	mkdir -p $(BUILD)/naructl
	cd naructl && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o $(BUILD)/naructl/naructl ./cmd/naructl
	cd tooling/qemu && go run ./cmd/naru-release -root $(CURDIR) -image $(IMAGE) -disk $(IMAGE) -guest $(GUEST) -naructl $(BUILD)/naructl/naructl -out $(RELEASE) -version $(VERSION)

# test-qemu boots Nodes of the image under QEMU, as the user, and reads their consoles. It needs
# /dev/kvm with nested virtualization and the host's mkfs.ext4 (for the data disk), and fails,
# never skips, when no test ran. It runs on a test image: the release's image with a throwaway
# root appended to the bundle (TEST_ROOT, ADR 0026), made here and read by the test that has a
# Node trust a root through the bundle. `make image` and `make release` build the image with no
# such root.
TESTROOT := $(BUILD)/testroot
test-qemu:
	@set -e; \
	(cd tooling/qemu && go run ./cmd/naru-testroot -out $(TESTROOT)); \
	$(MAKE) --no-print-directory disk image-upgrade TEST_ROOT=$(TESTROOT)/ca.pem; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd tooling/qemu && NARU_ROOT=$(CURDIR) NARU_IMAGE_DIR=$(IMAGE) NARU_GUEST_DIR=$(GUEST) NARU_UPGRADE_DIR=$(UPGRADE) NARU_TESTROOT=$(TESTROOT) go test -tags qemu_real -count=1 -v -timeout 60m -run '^Test(Node|ControlPlane)_' ./tests) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	n=$$(grep -Ec '^--- PASS: Test(Node|ControlPlane)_' "$$log" || true); \
	echo "$@: $$n test(s) passed, selected by -run '^Test(Node|ControlPlane)_' with -tags qemu_real"; \
	if [ "$$n" -eq 0 ]; then echo "$@: no test ran" >&2; exit 1; fi

# test-harness drives naru-cluster as an external harness does (docs/architecture/harness.md): from
# a directory outside the checkout, against the output of `make release`, with the gaon binaries
# of the pin as -gaon-bin. It is a target of its own so that test-qemu stays the length it is. It
# needs what test-qemu needs, and fails, never skips, when no test ran.
test-harness: release
	@set -e; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd tooling/qemu && NARU_RELEASE_DIR=$(RELEASE) NARU_GAON_DIR=$(GAON) go test -tags qemu_real -count=1 -v -timeout 30m -run '^TestCluster_' ./tests/harness) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	n=$$(grep -c '^--- PASS: TestCluster_' "$$log" || true); \
	echo "$@: $$n test(s) passed, selected by -run '^TestCluster_' with -tags qemu_real"; \
	if [ "$$n" -eq 0 ]; then echo "$@: no test ran" >&2; exit 1; fi

ci: lint format test build
