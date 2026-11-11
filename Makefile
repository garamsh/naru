.PHONY: lint format format-apply test test-cgroup test-qemu nodes judge-serial release build image disk image-repro ci

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

# naru-init is static: it runs in an initramfs with no libc.
build:
	$(call each_module, $(count_packages); (cd "$$dir" && go build ./...))
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/naru-init ./init/cmd/naru-init
	@echo "build: $(BIN)/naru-init: $$(file -b $(BIN)/naru-init | cut -d, -f1-4)"

# image builds the host kernel, the guest kernels and initrds, and the initramfs
# that carries them, from pinned sources, into $(IMAGE): bzImage,
# initramfs.cpio.gz, inputs.env (the Firecracker pin the tests read) and guest/,
# which holds what the initramfs carries of the guests, with guest.json naming
# it. The scripts skip what their inputs say is built, and verify every download.
GUEST := $(IMAGE)/guest
image:
	image/kernel/build.sh $(BUILD)/kernel $(IMAGE)
	image/mvmd-checkout.sh $(BUILD)/initramfs/mvmd
	image/guest/build.sh $(BUILD)/initramfs/mvmd $(BUILD)/kernel $(BUILD)/guest $(GUEST)
	image/initramfs/build.sh $(BUILD)/initramfs $(BUILD)/kernel $(IMAGE) $(GUEST)

# disk builds the UEFI disk image from image's outputs: naru.img, a raw GPT
# disk whose ESP holds the kernel as \EFI\BOOT\BOOTX64.EFI and the initramfs,
# and naru.qcow2, the same disk for Proxmox's import_from. It needs mkfs.fat,
# sgdisk and qemu-img, and neither root nor mtools.
disk: image
	image/disk/build.sh $(BUILD)/disk $(IMAGE)

# image-repro builds the image and the disk twice from clean build directories
# and fails unless the kernel, the initramfs, the guest manifest (which names
# the SHA-256 of every guest file), naru.img and naru.qcow2 of both are
# byte-identical. It takes two full kernel builds, two guest kernel builds and
# two builds of the initrds.
image-repro:
	@set -e; \
	for i in 1 2; do \
	  rm -rf $(BUILD)/repro/$$i; \
	  image/kernel/build.sh $(BUILD)/repro/$$i/kernel $(BUILD)/repro/$$i/image >/dev/null; \
	  image/mvmd-checkout.sh $(BUILD)/repro/$$i/initramfs/mvmd; \
	  image/guest/build.sh $(BUILD)/repro/$$i/initramfs/mvmd $(BUILD)/repro/$$i/kernel $(BUILD)/repro/$$i/guest $(BUILD)/repro/$$i/image/guest >/dev/null; \
	  image/initramfs/build.sh $(BUILD)/repro/$$i/initramfs $(BUILD)/repro/$$i/kernel $(BUILD)/repro/$$i/image $(BUILD)/repro/$$i/image/guest >/dev/null; \
	  image/disk/build.sh $(BUILD)/repro/$$i/disk $(BUILD)/repro/$$i/image >/dev/null; \
	done; \
	for f in bzImage initramfs.cpio.gz guest/guest.json naru.img naru.qcow2; do \
	  a=$$(sha256sum $(BUILD)/repro/1/image/$$f | cut -d' ' -f1); b=$$(sha256sum $(BUILD)/repro/2/image/$$f | cut -d' ' -f1); \
	  echo "$@: $$f build 1 $$a"; echo "$@: $$f build 2 $$b"; \
	  if [ "$$a" != "$$b" ]; then echo "$@: $$f differs between the builds" >&2; exit 1; fi; \
	done; \
	echo "$@: bzImage, initramfs.cpio.gz, guest.json, naru.img and naru.qcow2 are byte-identical across two clean builds"

# nodes boots N (default 2) Nodes of the image under QEMU on one network, as
# the user, and keeps them up until interrupted. Their disks and consoles are
# under $(BUILD)/nodes.
N ?= 2
nodes: disk
	rm -rf $(BUILD)/nodes
	cd tooling/qemu && go run ./cmd/naru-nodes -n $(N) -root $(CURDIR) -image $(IMAGE) -guest $(GUEST) -dir $(BUILD)/nodes

# judge-serial reads the serial logs of two Nodes, node-1 first, and exits 0 only when they show
# what the QEMU tests assert (tooling/qemu/internal/judge). The logs are raw captures: Proxmox's
# termproxy or `qm terminal`, or the console.log of a `make nodes` run started with -probe.
judge-serial:
	@test -n "$(NODE1)" -a -n "$(NODE2)" || { echo "judge-serial: usage: make judge-serial NODE1=<log> NODE2=<log>" >&2; exit 2; }
	cd tooling/qemu && go run ./cmd/naru-judge -node1 "$(abspath $(NODE1))" -node2 "$(abspath $(NODE2))"

# release writes the assets of a deployment into $(RELEASE): naru-<version>.qcow2 (the boot disk),
# naru-data-<version>.qcow2 (the one data disk both Nodes attach), naru-node-1.yaml and
# naru-node-2.yaml (example user-data for Proxmox) and SHA256SUMS. The version is
# `git describe --tags --always`, or VERSION=.
VERSION ?= $(shell git describe --tags --always)
RELEASE := $(BUILD)/release
release: disk
	rm -rf $(RELEASE)
	cd tooling/qemu && go run ./cmd/naru-release -root $(CURDIR) -image $(IMAGE) -disk $(IMAGE) -guest $(GUEST) -out $(RELEASE) -version $(VERSION)

# test-qemu boots two Nodes of the image under QEMU, as the user, and reads
# their consoles. It needs /dev/kvm with nested virtualization and the host's
# mke2fs, and fails, never skips, when no test ran.
test-qemu: disk
	@set -e; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd tooling/qemu && NARU_ROOT=$(CURDIR) NARU_IMAGE_DIR=$(IMAGE) NARU_GUEST_DIR=$(GUEST) go test -tags qemu_real -count=1 -v -timeout 20m -run '^TestNode_' ./tests) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	n=$$(grep -c '^--- PASS: TestNode_' "$$log" || true); \
	echo "$@: $$n test(s) passed, selected by -run '^TestNode_' with -tags qemu_real"; \
	if [ "$$n" -eq 0 ]; then echo "$@: no test ran" >&2; exit 1; fi

ci: lint format test build
