.PHONY: lint lint-imports lint-modules format format-apply test test-cgroup test-qemu test-fleet test-count test-harness test-release-upgrade nodes judge-serial release build image image-upgrade disk image-repro source-archive in-builder-image in-builder-image-upgrade in-builder-disk in-builder-release ci

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

# govulncheck is run from source at this version: the module checksum database verifies it. Moved
# by the PM with the other pins (.github/workflows/ci.yml).
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0

# lint is go vet over every module (the second pass vets the files only the QEMU tests build),
# shellcheck over the scripts of image/ (it must be installed; the GitHub-hosted runner has it), and
# govulncheck over every module, with the QEMU files included. No golangci-lint is configured: the
# project holds no //nolint directive (docs/architecture/structure.md).
lint:
	$(call each_module, $(count_packages); (cd "$$dir" && go vet ./... && go vet -tags qemu_real ./...))
	@set -e; \
	command -v shellcheck >/dev/null || { echo "$@: shellcheck is not installed" >&2; exit 1; }; \
	n=$$(git ls-files --cached --others --exclude-standard -z -- 'image/*.sh' | tr -cd '\0' | wc -c); \
	echo "$@: $$(shellcheck --version | sed -n 2p): $$n script(s) selected by git ls-files --cached --others --exclude-standard 'image/*.sh'"; \
	if [ "$$n" -eq 0 ]; then echo "$@: no script under image/" >&2; exit 1; fi; \
	git ls-files --cached --others --exclude-standard -z -- 'image/*.sh' | xargs -0 shellcheck -x -P SCRIPTDIR
	$(call each_module, (cd "$$dir" && go run $(GOVULNCHECK) -tags qemu_real ./...))

# lint-imports fails when a unit imports another unit, when lib/ imports a unit
# (docs/architecture/structure.md), or when a module under the root is not in go.work's use list.
# It reads every Go file of every module under the root, and prints how many.
lint-imports:
	cd tooling/importcheck && go run ./cmd/importcheck -root $(CURDIR)

# lint-modules fails when two modules of go.work require one dependency at different versions, or a
# go.mod's go directive is not go.work's. It prints how many dependencies it compared.
lint-modules:
	cd tooling/importcheck && go run ./cmd/modcheck -root $(CURDIR)

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
	(cd lib/init && go test -c -o "$$bin/linux.test" ./linux); \
	log=$$(mktemp); trap 'rm -rf "$$bin" "$$log"' EXIT; \
	if ! systemd-run --user --scope --quiet -p Delegate=yes sh -c \
	  'NARU_TEST_CGROUP_ROOT=/sys/fs/cgroup$$(cut -d: -f3 /proc/self/cgroup) exec "$$0" -test.v -test.run "^TestCgroups_" -test.count=1' \
	  "$$bin/linux.test" >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	n=$$(grep -c '^--- PASS: TestCgroups_' "$$log" || true); \
	echo "$@: $$n test(s) passed, selected by -test.run '^TestCgroups_' in a delegated cgroup"; \
	if [ "$$n" -eq 0 ]; then echo "$@: no test ran" >&2; exit 1; fi

# The two naru-init, naru-signer and naru-orchd are static: each is carried by an initramfs with no libc. naructl is static too: it is a release
# asset an operator runs on any Linux amd64 host.
build:
	$(call each_module, $(count_packages); (cd "$$dir" && GOWORK=off go build ./...))
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/host/naru-init ./host/cmd/naru-init
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/node/naru-init ./node/cmd/naru-init
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/host/naru-signer ./host/cmd/naru-signer
	CGO_ENABLED=0 go build -trimpath -o $(BIN)/orch/naru-orchd ./orch/cmd/naru-orchd
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -o $(BIN)/naructl ./naructl/cmd/naructl
	@echo "build: $(BIN)/host/naru-init: $$(file -b $(BIN)/host/naru-init | cut -d, -f1-4)"
	@echo "build: $(BIN)/node/naru-init: $$(file -b $(BIN)/node/naru-init | cut -d, -f1-4)"
	@echo "build: $(BIN)/host/naru-signer: $$(file -b $(BIN)/host/naru-signer | cut -d, -f1-4)"
	@echo "build: $(BIN)/orch/naru-orchd: $$(file -b $(BIN)/orch/naru-orchd | cut -d, -f1-4)"
	@echo "build: $(BIN)/naructl: $$(file -b $(BIN)/naructl | cut -d, -f1-4)"

# The image targets run in the builder container (docs/architecture/image.md §The builder, ADR 0063):
# image, image-upgrade, disk, release and image-repro start image/builder/run.sh, which runs the
# target of the same name with the in-builder- prefix in the container with the repository mounted
# at /naru. Those prefixed targets are the recipes; run them by hand only inside the builder.
# in_builder runs one of them with the variables the recipes read. A path given in a variable must
# be inside the repository, and the container sees it under /naru.
BUILDER := image/builder/run.sh
container_path = $(if $(1),$(if $(filter $(CURDIR)/%,$(abspath $(1))),/naru/$(patsubst $(CURDIR)/%,%,$(abspath $(1))),$(error $(1) is outside the repository, which is all the builder container sees)))
in_builder = $(BUILDER) make $(1) VERSION='$(VERSION)' TEST_ROOT='$(call container_path,$(TEST_ROOT))' PREV_MANIFEST='$(call container_path,$(PREV_MANIFEST))' RECREATE='$(RECREATE)' $(2)

# The packages of image/pkgs are the build (ADR 0075): every program, kernel and archive of the image
# is one package, built from pinned sources in BuildKit by image/pkgs/build.sh, which needs the Docker
# daemon and so runs on the host before the builder container starts. in-builder-image lays them out
# into $(IMAGE) (image/pkgs/layout.sh): bzImage (the kernel with the initramfs inside it, ADR 0020;
# bzImage.bare is the kernel before that), initramfs.cpio.gz, node-initrd.cpio.gz, boot-sets.json,
# inputs.env (the Firecracker pin the tests read), guest/ (guest.json and what it names, which the
# initramfs carries of the guests), extensions/ (the naru-orch system extension, ADR 0055), and the
# directories of programs the tests and the release take: gaon/, etcd/, k8s/ (Kubernetes, containerd,
# runc and the CNI plugins) and regtools/ (the credential helpers, ADR 0026, and the root bundle).
GUEST := $(IMAGE)/guest
GAON := $(IMAGE)/gaon
# TEST_ROOT, empty for the release, is the PEM file of a throwaway root appended to the bundle: the
# test image's, which test-qemu builds and no release carries.
TEST_ROOT ?=
# PKGS holds the packages as OCI tars, which image/pkgs/build.sh writes, and PKGROOT what naru-pkg
# unpacks from them.
PKGS := $(BUILD)/pkgs
# GOBUILD builds one static program of this repository with the builder's Go: nothing but the flags and
# the sources enters the bytes.
GOBUILD := CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -trimpath -buildvcs=false -ldflags='-s -w'
PKGROOT := $(BUILD)/pkgroot
# PKGS_GROUPS names the groups of image/pkgs/docker-bake.hcl that build.sh builds: default is what
# make image needs and upgrade adds the kernels of make image-upgrade.
PKGS_GROUPS := default
image-upgrade: PKGS_GROUPS := default upgrade
# VERSION is the version naru-init reports on the Node API and the release assets carry: `git describe
# --tags --always`, or VERSION=.
VERSION ?= $(shell git describe --tags --always)
export NARU_VERSION := $(VERSION)
# source-archive writes the git archive of HEAD that the release's sources tar carries (ADR 0076). It runs on the
# host: a worktree's .git is outside the builder container's view of the repository.
SOURCE_ARCHIVE := $(CURDIR)/build/source.tar
source-archive:
	mkdir -p $(dir $(SOURCE_ARCHIVE))
	git archive --format=tar -o $(SOURCE_ARCHIVE) HEAD

release: source-archive
image image-upgrade disk release:
	NARU_TEST_ROOT='$(if $(TEST_ROOT),$(abspath $(TEST_ROOT)))' image/pkgs/build.sh $(PKGS) $(PKGS_GROUPS)
	$(call in_builder,in-builder-$@)

in-builder-image:
	cd tooling/disk && $(GOBUILD) -o $(abspath $(BUILD))/naru-pkg ./cmd/naru-pkg
	NARU_TEST_ROOT='$(TEST_ROOT)' image/pkgs/layout.sh $(BUILD)/naru-pkg $(PKGS) $(PKGROOT) $(IMAGE)

# in-builder-image-upgrade lays out the two kernels the upgrade tests (ADR 0025) put on a running Node,
# which the upgrade group of image/pkgs builds from the programs and the kernel of the image:
# next/bzImage is the same tree with the version $(VERSION)-next stamped in, and refuse/bzImage a
# naru-init that cannot find its data disk (the linker flag is REFUSE_LDFLAGS of the bake file), so a
# trial boot of it refuses, with a seed or without, and rolls back by itself.
UPGRADE := $(BUILD)/upgrade
in-builder-image-upgrade: in-builder-image
	@set -e; \
	for v in next refuse; do \
	  rm -rf $(UPGRADE)/$$v; mkdir -p $(UPGRADE)/$$v; \
	  $(BUILD)/naru-pkg unpack $(PKGROOT) $(PKGS)/bzImage-$$v.tar; \
	  cp $(PKGROOT)/bzImage-$$v/bzImage $(UPGRADE)/$$v/; \
	done

# in-builder-disk builds the UEFI disk image from image's outputs: naru.img, a raw GPT
# disk whose ESP holds the kernel as \EFI\BOOT\BOOTX64.EFI and has room for a second one, and
# naru.qcow2, the same disk for Proxmox's import_from. It needs mkfs.fat,
# sgdisk and qemu-img, and neither root nor mtools.
in-builder-disk: in-builder-image
	image/host/disk/build.sh $(BUILD)/disk $(IMAGE)

# image-repro shows that the release assets do not depend on the machine. Without REFERENCE it runs
# `make release` twice, in two builder containers with empty build directories, and fails unless the
# SHA256SUMS of both agree (image/repro.sh names the assets that are not reproducible, and why).
# With REFERENCE=<the SHA256SUMS of a release another machine built from this commit> and the
# VERSION of that release, it builds once and compares with that file: `gh release download <tag>
# --pattern SHA256SUMS` fetches the release workflow's.
REFERENCE ?=
image-repro:
	@set -e; \
	$(MAKE) source-archive; \
	if [ -n "$(REFERENCE)" ]; then \
	  $(MAKE) release; \
	  image/repro.sh "$(REFERENCE)" $(RELEASE)/SHA256SUMS; \
	else \
	  for i in 1 2; do \
	    rm -rf $(BUILD)/repro/$$i; \
	    NARU_PKGS_NO_CACHE=1 image/pkgs/build.sh $(BUILD)/repro/$$i/pkgs; \
	    $(call in_builder,in-builder-release,BUILD=/naru/build/repro/$$i); \
	  done; \
	  image/repro.sh $(BUILD)/repro/1/release/SHA256SUMS $(BUILD)/repro/2/release/SHA256SUMS $(BUILD)/repro/1/pkgs $(BUILD)/repro/2/pkgs; \
	fi

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

# in-builder-release writes the assets of a deployment into $(RELEASE): naru-host-<version>.qcow2 (the boot disk),
# naru-host-upgrade-<version>.tar (the upgrade bundle: the kernel and a manifest of its digest),
# naru-node-kernel-<version>, naru-node-initramfs-<version> and naru-node-<version>.json (the Node boot
# set a host takes with `naructl host boot-set add`, ADR 0046), naru-data-<version>.qcow2 (the one data
# disk both Nodes attach), naru-guest-<version>.json,
# naru-inputs-<version>.json (what naru-cluster reads besides the disks), naructl-<version>-linux-amd64
# (the operator's tool, which also writes the test user-data below from a CA derived from the version (ADR 0078) that lives only for the
# run), naru-cluster-<version>-linux-amd64 (the harness's command, a static binary, ADR 0074), naru-node-1.test.yaml and naru-node-2.test.yaml (test-only user-data for Proxmox: they carry
# gaon's keys, so they are for a throwaway deployment) and SHA256SUMS. $(BUILD)/release-figures.env,
# outside $(RELEASE) because every file there is an asset, holds the ESP size and the largest in-place
# upgrade kernel, which the release workflow puts in the notes. VERSION is defined above image.
# PREV_MANIFEST is the path of the manifest.json of the previous release's upgrade bundle: the
# release is refused when its kernel would not fit the trial slot of a Node installed from that
# release's image (ADR 0037), unless RECREATE=1, which says the release's notes tell those Nodes to
# be recreated. Without PREV_MANIFEST nothing is checked, and the command says so.
RELEASE := $(BUILD)/release
PREV_MANIFEST ?=
RECREATE ?=
in-builder-release: in-builder-disk
	rm -rf $(RELEASE)
	mkdir -p $(BUILD)/naructl
	rm -rf $(PKGROOT)/naru
	$(BUILD)/naru-pkg unpack $(PKGROOT) $(PKGS)/naru.tar
	cd naructl && $(GOBUILD) -o $(abspath $(BUILD))/naructl/naructl ./cmd/naructl
	cd tooling/qemu && $(GOBUILD) -o $(abspath $(BUILD))/naructl/naru-cluster ./cmd/naru-cluster
	cd tooling/qemu && go run ./cmd/naru-release -root $(CURDIR) -image $(IMAGE) -disk $(IMAGE) -guest $(GUEST) -naructl $(BUILD)/naructl/naructl -naru-cluster $(BUILD)/naructl/naru-cluster -out $(RELEASE) -version $(VERSION) -notes-figures $(BUILD)/release-figures.env -epoch $$(. $(CURDIR)/image/build.env && echo $$SOURCE_DATE_EPOCH) -binaries $(IMAGE)/gaon,$(IMAGE)/etcd,$(IMAGE)/k8s,$(IMAGE)/regtools,$(PKGROOT)/naru/usr/bin -archives $(IMAGE)/initramfs.cpio.gz,$(IMAGE)/node-initrd.cpio.gz -source-archive $(SOURCE_ARCHIVE) $(if $(PREV_MANIFEST),-prev-manifest $(abspath $(PREV_MANIFEST))) $(if $(RECREATE),-requires-recreation)

# test-qemu boots Nodes of the image under QEMU, as the user, and reads their consoles. It needs
# /dev/kvm with nested virtualization and the host's mkfs.ext4 (for the data disk), and fails,
# never skips, when no test ran. It runs on a test image: the release's image with a throwaway
# root appended to the bundle (TEST_ROOT, ADR 0026), made here and read by the test that has a
# Node trust a root through the bundle. `make image` and `make release` build the image with no
# such root.
# kubectl is a precondition, checked before anything is built: two of the tests drive a cluster with
# it.
TESTROOT := $(BUILD)/testroot
test-qemu:
	@set -e; \
	command -v kubectl >/dev/null || { echo "$@: kubectl is not installed; two tests need it" >&2; exit 1; }; \
	(cd tooling/qemu && go run ./cmd/naru-testroot -out $(TESTROOT)); \
	$(MAKE) --no-print-directory disk image-upgrade TEST_ROOT=$(TESTROOT)/ca.pem; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd tooling/qemu && NARU_ROOT=$(CURDIR) NARU_IMAGE_DIR=$(IMAGE) NARU_GUEST_DIR=$(GUEST) NARU_UPGRADE_DIR=$(UPGRADE) NARU_TESTROOT=$(TESTROOT) go test -tags qemu_real -count=1 -v -timeout 60m -run '^Test(Node|ControlPlane)_' ./tests) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	$(MAKE) --no-print-directory test-count TARGET=$@ LOG="$$log" PATTERN='Test(Node|ControlPlane)_'

# test-fleet runs the TestFleet_ scenarios: several Nodes, hosts and an orch under QEMU, on the
# image test-qemu runs on. It has a timeout of its own and the same count check as test-qemu.
test-fleet:
	@set -e; \
	(cd tooling/qemu && go run ./cmd/naru-testroot -out $(TESTROOT)); \
	$(MAKE) --no-print-directory disk image-upgrade TEST_ROOT=$(TESTROOT)/ca.pem; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd tooling/qemu && NARU_ROOT=$(CURDIR) NARU_IMAGE_DIR=$(IMAGE) NARU_GUEST_DIR=$(GUEST) NARU_UPGRADE_DIR=$(UPGRADE) NARU_TESTROOT=$(TESTROOT) go test -tags qemu_real -count=1 -v -timeout 90m -run '^TestFleet_' ./tests) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	$(MAKE) --no-print-directory test-count TARGET=$@ LOG="$$log" PATTERN='TestFleet_'

# test-count judges the log of a `go test -v` run of the QEMU tests, for the targets above and
# below: it prints how many tests matching PATTERN passed and how many skipped, and fails when none
# passed or when a test skipped that SKIP_ALLOW (test names, space-separated) does not name. Nothing
# is allowed to skip now. Usage: make test-count TARGET=<name> LOG=<file> PATTERN=<test name prefix>
SKIP_ALLOW :=
test-count:
	@test -n "$(TARGET)" -a -n "$(LOG)" -a -n "$(PATTERN)" || { echo "usage: make $@ TARGET=<name> LOG=<go test -v log> PATTERN=<test name prefix, as -run takes it without ^>" >&2; exit 2; }
	@set -e; \
	pass=$$(grep -Ec '^--- PASS: $(PATTERN)' "$(LOG)" || true); \
	skipped=$$(grep -E '^ *--- SKIP: $(PATTERN)' "$(LOG)" | sed -E 's/^ *--- SKIP: ([^ ]+).*/\1/' || true); \
	skip=0; bad=""; \
	for s in $$skipped; do \
	  skip=$$((skip+1)); \
	  case " $(SKIP_ALLOW) " in *" $$s "*) ;; *) bad="$$bad $$s" ;; esac; \
	done; \
	echo "$(TARGET): $$pass test(s) passed, $$skip skipped, selected by -run '^$(PATTERN)' with -tags qemu_real"; \
	if [ -n "$$bad" ]; then echo "$(TARGET): skipped, and not in SKIP_ALLOW:$$bad" >&2; exit 1; fi; \
	if [ "$$pass" -eq 0 ]; then echo "$(TARGET): no test ran" >&2; exit 1; fi

# test-release-upgrade installs the release in FROM and upgrades the Node in place to the release in
# TO, both directories as `gh release download <tag>` writes them (docs/architecture/image.md). It
# is run before a release is cut, on the previous release and the candidate. It needs what test-qemu
# needs, builds nothing and fails, never skips, when no test passed.
test-release-upgrade:
	@test -n "$(FROM)" -a -n "$(TO)" || { echo "usage: make $@ FROM=<release dir> TO=<release dir>" >&2; exit 2; }
	@set -e; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd tooling/qemu && NARU_FROM_RELEASE_DIR=$(abspath $(FROM)) NARU_TO_RELEASE_DIR=$(abspath $(TO)) go test -tags qemu_real -count=1 -v -timeout 30m -run '^TestRelease_' ./tests) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	$(MAKE) --no-print-directory test-count TARGET=$@ LOG="$$log" PATTERN='TestRelease_'

# test-harness drives naru-cluster as an external harness does (docs/architecture/harness.md): from
# a directory outside the checkout, against the output of `make release`, with the gaon binaries
# of the pin as -gaon-bin. With RELEASE=<dir> on the command line it builds nothing and runs the
# release directory's naru-cluster-<version>-linux-amd64, after checking it against SHA256SUMS, and
# fails when the directory has none; without it, `make release` runs first and the test builds
# naru-cluster from the workspace. It is a target of its own so that test-qemu stays the length it
# is. It needs what test-qemu needs, and fails, never skips, when no test ran.
HARNESS_FROM := $(filter command line,$(origin RELEASE))
test-harness: $(if $(HARNESS_FROM),,release)
	@set -e; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd tooling/qemu && NARU_RELEASE_DIR=$(abspath $(RELEASE)) NARU_GAON_DIR=$(GAON) NARU_HARNESS_RELEASE_BINARY=$(if $(HARNESS_FROM),1) go test -tags qemu_real -count=1 -v -timeout 30m -run '^TestCluster_' ./tests/harness) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	$(MAKE) --no-print-directory test-count TARGET=$@ LOG="$$log" PATTERN='TestCluster_'

ci: lint lint-imports lint-modules format test build
