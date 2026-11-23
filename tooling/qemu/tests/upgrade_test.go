//go:build qemu_real

package tests_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

// The tests of the in-place upgrade (ADR 0025) run one Node on the boot disk the build made, with
// its ESP writable and the Node API on, and send it a bundle with the naructl of the checkout. The
// bundles hold the kernels `make image-upgrade` builds: the same tree stamped with another
// version, and a naru-init that cannot find its configuration drive. QEMU runs with -no-reboot, so
// a guest that reboots ends its QEMU process, and the test starts the Node again on its disks, as a
// hypervisor does; nothing else is done to the Node, whose NVRAM persists as a real machine's does.

// upgradeTimeout is how long a Node may take over a trial boot, its judgement and the boot after.
const upgradeTimeout = 6 * time.Minute

// storeFile is the Node's stored configuration on the data disk.
const storeFile = wire.DataMount + "/config/store.yaml"

// startUpgradeNode boots a one-Node cluster that runs gaon, with the Node API, on a copy of the boot
// disk of the build with its ESP writable, with the user-data of the naructl of the checkout. It
// returns the Node and the admin's client file once the Node answers.
func startUpgradeNode(t *testing.T) (*node.Node, string) {
	t.Helper()
	return startUpgradeNodeOn(t, tool, inputs, copyBootDisk(t), addNothing)
}

// startUpgradeNodeOn is startUpgradeNode for the naructl bin, which writes the authorities, the
// admin's client and the user-data, the inputs the data disk carries, and the boot disk to boot,
// which the Node writes to. configure sets what the case adds to the Node's configuration and to
// the host `gen node` writes the user-data of, as node.ClusterSpec.Configure does for a cluster;
// the plain case passes addNothing.
func startUpgradeNodeOn(t *testing.T, bin naructl.Tool, inputs fixture.Inputs, disk string, configure func(*node.Config, *naructl.Node)) (*node.Node, string) {
	t.Helper()
	dir := t.TempDir()
	caDir := filepath.Join(dir, "ca")
	apiPort, hostPort := freeLoopbackPort(t), freeLoopbackPort(t)
	admin := filepath.Join(dir, "admin.yaml")
	require.NoError(t, bin.GenCA(caDir))
	require.NoError(t, bin.GenAPICA(caDir))
	require.NoError(t, bin.GenAPIClient(caDir, "admin", "127.0.0.1:"+strconv.Itoa(apiPort), admin))
	guest := filepath.Join(dir, "guest.json")
	manifest, err := json.Marshal(inputs.Guest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(guest, manifest, 0o600))

	cfg := node.Config{Name: "node-1", Index: 1, Total: 1, ControlPlane: true, Inputs: inputs, HostPort: hostPort, APIHostPort: apiPort}
	n := node.QEMUNetwork(1, 1)
	ones, _ := net.IPMask(net.ParseIP(n.Netmask).To4()).Size()
	nodeDir := filepath.Join(dir, "node-1")
	require.NoError(t, os.MkdirAll(nodeDir, 0o755))
	gen := naructl.Node{
		CA: caDir, Name: "node-1", ControlPlane: true, PairsAsFiles: true,
		Address: n.IP + "/" + strconv.Itoa(ones), MAC: n.MAC, Gateway: n.Gateway, Nameservers: n.Nameservers, Search: n.Search,
		APIServer: n.IP, MachinePrefix: n.MachinePrefix.String(), Guest: guest, Firecracker: inputs.FirecrackerSHA256,
		ServingAddresses: []string{"127.0.0.1"}, ExtraNIC: cfg.HostMAC() + "=" + node.HostNICPrefix, Binaries: map[string]string{},
		Out: filepath.Join(nodeDir, "naru.yaml"), NetworkConfig: filepath.Join(nodeDir, "network-config"),
	}
	configure(&cfg, &gen)
	require.NoError(t, bin.GenNode(gen))
	userData, err := os.ReadFile(gen.Out)
	require.NoError(t, err)
	networkConfig, err := os.ReadFile(gen.NetworkConfig)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(caDir), "the authorities' keys are not kept")

	nd, err := node.Start(node.Spec{
		Config: cfg, Disk: disk, WritableDisk: true, Dir: nodeDir,
		Mcast: fmt.Sprintf("230.78.0.1:%d", 20000+rand.IntN(40000)), NaruYAML: userData, NetworkConfig: networkConfig,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		nd.Stop()
		t.Logf("console of %s:\n%s", nd.Name, nd.Console())
	})
	eventually(t, "the Node API answers an admin", func() bool { return ctlWith(bin, admin, "version").Err == nil })
	return nd, admin
}

// addNothing is the configure of a Node with nothing added.
func addNothing(*node.Config, *naructl.Node) {}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// bundleFor writes the upgrade bundle of the kernel that make image-upgrade built into dir/<kind>,
// as the release would, and its SHA256SUMS, and returns their paths.
func bundleFor(t *testing.T, kind, version string) (bundle, sums string) {
	t.Helper()
	return bundleOf(t, filepath.Join(os.Getenv("NARU_UPGRADE_DIR"), kind, "bzImage"), version)
}

// bundleWithExtensionFor is bundleFor with the naru-orch extension that make image-upgrade built for
// the kernel's version, as the release's bundle carries it (ADR 0084). It also returns the extension's
// SHA-256, which `naructl extension list` shows.
func bundleWithExtensionFor(t *testing.T, kind, version string) (bundle, sums, extSHA string) {
	t.Helper()
	dir := os.Getenv("NARU_UPGRADE_DIR")
	image := filepath.Join(dir, kind, fixture.ExtensionDir, fixture.ExtensionImageName)
	raw, err := os.ReadFile(image)
	require.NoError(t, err)
	bundle, sums = bundleOf(t, filepath.Join(dir, kind, "bzImage"), version, release.BundleExtension{Name: fixture.ExtensionName, Path: image})
	return bundle, sums, fmt.Sprintf("%x", sha256.Sum256(raw))
}

// upgradeState is what `naructl upgrade --status` prints.
type upgradeState struct{ Version, BootID, Slot, Outcome, Reason string }

func statusOf(bin naructl.Tool, admin string) (upgradeState, error) {
	r := ctlWith(bin, admin, "upgrade", "--status")
	if r.Err != nil {
		return upgradeState{}, r.Err
	}
	return parseUpgradeState(r.Stdout)
}

// parseUpgradeState reads what `naructl upgrade --status` printed.
func parseUpgradeState(stdout string) (upgradeState, error) {
	var s upgradeState
	for _, line := range strings.Split(stdout, "\n") {
		key, rest, _ := strings.Cut(line, " ")
		rest = strings.TrimSpace(rest)
		switch key {
		case "version":
			s.Version = rest
		case "boot":
			s.BootID = strings.TrimSpace(strings.TrimPrefix(rest, "ID"))
		case "slot":
			s.Slot = rest
		case "upgrade":
			s.Outcome = rest
		case "reason":
			s.Reason = rest
		}
	}
	return s, nil
}

// awaitRestarting waits for cond to hold, starting the Node again each time its QEMU process ends by
// itself, which is a reboot of the guest.
func awaitRestarting(t *testing.T, nd *node.Node, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(upgradeTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-nd.Done():
			require.NoError(t, nd.Reboot())
		default:
		}
		if cond() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s: not within %s", what, upgradeTimeout)
}

// storeOf is the stored configuration as the Node's data disk holds it now.
func storeOf(t *testing.T, nd *node.Node) []byte {
	t.Helper()
	snap, err := nd.SnapshotData(filepath.Join(t.TempDir(), "data.img"))
	require.NoError(t, err)
	b, err := snap.Read(storeFile)
	require.NoError(t, err)
	return b
}

func TestNode_UpgradeReplacesTheKernelOfARunningNodeAndKeepsItWhenItIsHealthyWithItsDataIntact(t *testing.T) {
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	next := want + "-next"
	nd, admin := startUpgradeNode(t)
	before, err := statusOf(tool, admin)
	require.NoError(t, err)
	require.Equal(t, want, before.Version)
	require.Equal(t, "default", before.Slot)
	require.Equal(t, want, documentOf(t, admin).Status.Version, "the document and `version` agree before the upgrade")
	require.NoError(t, ctl(admin, "apply", "-f", patchFile(t, idle("1m"))).Err)
	require.Equal(t, 1, revisionOf(t, admin))
	stored := storeOf(t, nd)
	bundle, sums := bundleFor(t, "next", next)

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	assert.Contains(t, r.Stdout, "the Node accepted the upgrade")
	awaitRestarting(t, nd, "the Node runs the upgraded kernel and has promoted it", func() bool {
		s, err := statusOf(tool, admin)
		return err == nil && s.Version == next && s.Slot == "default" && strings.Contains(s.Outcome, "promoted")
	})
	after, err := statusOf(tool, admin)
	require.NoError(t, err)
	assert.Equal(t, next, after.Version, "the version changed")
	status := documentOf(t, admin).Status
	assert.Equal(t, next, status.Version, "the document reads the new version too")
	require.NotNil(t, status.LastUpgrade)
	assert.Equal(t, "promoted", status.LastUpgrade.Result, "and how the upgrade ended")
	assert.NotEqual(t, before.BootID, after.BootID, "an upgrade is a new boot")
	assert.Contains(t, after.Outcome, want+" -> "+next+", promoted")
	assert.Equal(t, 1, revisionOf(t, admin), "the store revision is the one before")
	assert.Equal(t, stored, storeOf(t, nd), "the file on the data disk reads back the same")
	assert.Contains(t, nd.Console(), "upgrade promoted")

	// Promotion is the rename of the slot: a power cycle boots the new kernel from the default slot.
	require.NoError(t, nd.Reboot())
	awaitRestarting(t, nd, "the new kernel boots from the default slot", func() bool {
		s, err := statusOf(tool, admin)
		return err == nil && s.Version == next && s.Slot == "default" && s.BootID != after.BootID
	})
	assert.NotContains(t, nd.Console(), "upgrade rolled back")
}

func TestNode_UpgradeToAKernelThatRefusesIsRolledBackByTheNodeToTheVersionBefore(t *testing.T) {
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	refusing := want + "-refuse"
	nd, admin := startUpgradeNode(t)
	before, err := statusOf(tool, admin)
	require.NoError(t, err)
	require.Equal(t, want, before.Version)
	require.NoError(t, ctl(admin, "apply", "-f", patchFile(t, idle("1m"))).Err)
	stored := storeOf(t, nd)
	bundle, sums := bundleFor(t, "refuse", refusing)

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	// Nothing is done to the Node from here: the trial kernel cannot find its data disk, init
	// gives up on it and reboots, and the default slot boots again.
	awaitRestarting(t, nd, "the Node is back on its old version and says the upgrade rolled back", func() bool {
		s, err := statusOf(tool, admin)
		return err == nil && strings.Contains(s.Outcome, "rolled back")
	})
	after, err := statusOf(tool, admin)
	require.NoError(t, err)
	assert.Equal(t, want, after.Version, "the Node runs the version before")
	assert.Equal(t, "default", after.Slot)
	assert.NotEqual(t, before.BootID, after.BootID, "the trial and the boot after it are new boots")
	assert.Contains(t, after.Outcome, want+" -> "+refusing+", rolled back")
	assert.Contains(t, after.Reason, "the boot sequence refused: ", "the outcome says why, in init's words")
	assert.Contains(t, nd.Console(), "upgrade rolled back")
	assert.Equal(t, stored, storeOf(t, nd), "the data disk is as it was")
}

// startUpgradeNodeMounting is startUpgradeNode for a Node whose spec.extensions names the
// extension the data disk stores, which is built for the version the Node runs: the entry of a
// control host before its upgrade (ADR 0055).
func startUpgradeNodeMounting(t *testing.T) (*node.Node, string) {
	t.Helper()
	return startUpgradeNodeNaming(t, "{name: naru-orch, sha256: "+inputs.Extension.SHA256+"}")
}

// startUpgradeNodeNaming is startUpgradeNodeMounting for the entry of spec.extensions given as YAML.
// An entry with no sha256 is resolved by init to the image built for its version (ADR 0084).
func startUpgradeNodeNaming(t *testing.T, entry string) (*node.Node, string) {
	t.Helper()
	nd, admin := startUpgradeNode(t)
	ext := inputs.Extension
	patch := "spec:\n  extensions:\n    - " + entry + "\n"
	r := ctl(admin, "apply", "-f", patchFile(t, patch), "--mode", "staged")
	require.NoError(t, r.Err, r.refusal())
	require.NoError(t, nd.Reboot())
	eventually(t, "the Node mounts the extension at its boot", func() bool {
		rows, err := extensionTable(admin)
		row := rows[ext.SHA256]
		return err == nil && row != nil && row[len(row)-1] == "/usr/lib/naru/extensions/naru-orch"
	})
	return nd, admin
}

func TestNode_UpgradeIsRefusedByTheNodeWith422WhenAnExtensionEntryHasNoImageForTheBundlesVersion(t *testing.T) {
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	nd, admin := startUpgradeNodeMounting(t)
	before, err := statusOf(tool, admin)
	require.NoError(t, err)
	bundle, sums := bundleFor(t, "next", want+"-next")

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.Error(t, r.Err)
	assert.Contains(t, r.refusal(), "422", "the Node answers before it writes the slot")
	assert.Contains(t, r.refusal(), "naru-orch", "the refusal names the extension")
	assert.Contains(t, r.refusal(), "ADR 0084", "and the pinned-entry steps")
	after, err := statusOf(tool, admin)
	require.NoError(t, err)
	assert.Equal(t, before.BootID, after.BootID, "the Node was not rebooted")
	assert.Contains(t, after.Outcome, "none has ended", "and no upgrade started")
	assert.NotContains(t, nd.Console(), "upgrade staged")
}

// upgradableOrch boots a control Node that runs the signer and orch from the naru-orch extension its
// spec.extensions names without a digest, on a boot disk it may write, and has orch answer root. It
// returns the fleet and the Node API's admin file.
func upgradableOrch(t *testing.T) (*fleet, string) {
	t.Helper()
	f := newFleet(t, fleetOpts{controls: 1, upgradable: true})
	f.startOrch()
	return f, f.c.API.Admin
}

// awaitOrch waits for orch to answer root after the Node came back from an upgrade.
func awaitOrch(t *testing.T, f *fleet) {
	t.Helper()
	eventually(t, "orch answers root from the extension the Node mounted", func() bool {
		return f.orch("host", "pending").Err == nil
	})
}

func TestNode_UpgradeCarriesTheExtensionAnUnpinnedEntryNamesAndOrchAnswersFromItAfterwards(t *testing.T) {
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	next := want + "-next"
	f, admin := upgradableOrch(t)
	old := inputs.Extension.SHA256
	bundle, sums, newSHA := bundleWithExtensionFor(t, "next", next)
	require.NotEqual(t, old, newSHA, "the extension of the next version is another image")
	require.Contains(t, extensionRows(t, admin), old, "orch runs from the old image before the upgrade")

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	awaitRestarting(t, f.node(1), "the Node runs the next version and has promoted it", func() bool {
		s, err := statusOf(tool, admin)
		return err == nil && s.Version == next && s.Slot == "default" && strings.Contains(s.Outcome, "promoted")
	})
	eventually(t, "the Node mounts the extension the bundle carried and has removed the old one", func() bool {
		rows, err := extensionTable(admin)
		_, oldStored := rows[old]
		return err == nil && rows[newSHA] != nil && rows[newSHA][len(rows[newSHA])-1] == "/usr/lib/naru/extensions/naru-orch" && !oldStored
	})
	awaitOrch(t, f)
}

func TestNode_ARefusedUpgradeThatCarriedAnExtensionRollsBackToTheOldExtensionAndRemovesTheNewOne(t *testing.T) {
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	nd, admin := startUpgradeNodeNaming(t, "{name: naru-orch}")
	old := inputs.Extension.SHA256
	bundle, sums, refusedSHA := bundleWithExtensionFor(t, "refuse", want+"-refuse")

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	// The setup: the upgrade stored the refused image next to the old one, so its absence after the
	// rollback is the rollback's doing and not an upgrade that never stored it.
	staged := extensionRows(t, admin)
	require.Contains(t, staged, refusedSHA, "the upgrade stored the refused image before the reboot")
	require.Contains(t, staged, old, "the old image is still stored before the reboot")
	awaitRestarting(t, nd, "the Node is back on its version and has rolled the upgrade back", func() bool {
		s, err := statusOf(tool, admin)
		return err == nil && s.Version == want && s.Slot == "default" && strings.Contains(s.Outcome, "rolled back")
	})
	eventually(t, "the old extension is mounted and the refused version's is gone", func() bool {
		rows, err := extensionTable(admin)
		_, refusedStored := rows[refusedSHA]
		return err == nil && rows[old] != nil && rows[old][len(rows[old])-1] == "/usr/lib/naru/extensions/naru-orch" && !refusedStored
	})
}

func TestNode_ARefusedUpgradeThatCarriedAnExtensionLeavesOrchAnsweringFromTheOldExtension(t *testing.T) {
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	f, admin := upgradableOrch(t)
	old := inputs.Extension.SHA256
	bundle, sums, refusedSHA := bundleWithExtensionFor(t, "refuse", want+"-refuse")

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	staged := extensionRows(t, admin)
	require.Contains(t, staged, refusedSHA, "the upgrade stored the refused image before the reboot")
	require.Contains(t, staged, old, "the old image is still stored before the reboot")
	awaitRestarting(t, f.node(1), "the Node is back on its version and has rolled the upgrade back", func() bool {
		s, err := statusOf(tool, admin)
		return err == nil && s.Version == want && s.Slot == "default" && strings.Contains(s.Outcome, "rolled back")
	})
	eventually(t, "the old extension is mounted and the refused version's is gone", func() bool {
		rows, err := extensionTable(admin)
		_, refusedStored := rows[refusedSHA]
		return err == nil && rows[old] != nil && rows[old][len(rows[old])-1] == "/usr/lib/naru/extensions/naru-orch" && !refusedStored
	})
	awaitOrch(t, f)
}

func TestNode_UpgradeOfAHostWhoseRevisionZeroPinsAnExtensionIsRefusedUpFrontWithTheStepAndRollsBackNothing(t *testing.T) {
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	next := want + "-next"
	f := newFleet(t, fleetOpts{controls: 1, upgradable: true, pinned: true})
	f.startOrch()
	admin := f.c.API.Admin
	before, err := statusOf(tool, admin)
	require.NoError(t, err)
	bundle, sums, newSHA := bundleWithExtensionFor(t, "next", next)

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.Error(t, r.Err, "the pinned entry of the running configuration is counted though no revision is stored")
	assert.Contains(t, r.refusal(), "422", "refused before the slot is written, not rolled back after a trial")
	assert.Contains(t, r.refusal(), "naru-orch", "the refusal names the extension")
	assert.Contains(t, r.refusal(), "naructl apply --mode staged", "and the step")
	after, err := statusOf(tool, admin)
	require.NoError(t, err)
	assert.Equal(t, before.BootID, after.BootID, "the Node was not rebooted")
	rows, err := extensionTable(admin)
	require.NoError(t, err)
	assert.NotContains(t, rows, newSHA, "the image the refused bundle carried is not kept")

	// The control: the step the refusal names, and the same bundle upgrades the host.
	step := ctl(admin, "apply", "-f", patchFile(t, "spec:\n  extensions:\n    - {name: naru-orch}\n"), "--mode", "staged")
	require.NoError(t, step.Err, step.refusal())
	r = ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)
	require.NoError(t, r.Err, r.refusal())
	awaitRestarting(t, f.node(1), "the Node runs the next version and has promoted it", func() bool {
		s, err := statusOf(tool, admin)
		return err == nil && s.Version == next && s.Slot == "default" && strings.Contains(s.Outcome, "promoted")
	})
	awaitOrch(t, f)
}
