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
const storeFile = node.DataMount + "/config/store.yaml"

// startUpgradeNode boots a one-Node cluster that runs gaon, with the Node API, on a copy of the boot
// disk with its ESP writable. It returns the Node and the admin's client file once the Node answers.
func startUpgradeNode(t *testing.T) (*node.Node, string) {
	t.Helper()
	dir := t.TempDir()
	caDir := filepath.Join(dir, "ca")
	apiPort, hostPort := freeLoopbackPort(t), freeLoopbackPort(t)
	admin := filepath.Join(dir, "admin.yaml")
	require.NoError(t, tool.GenCA(caDir))
	require.NoError(t, tool.GenAPICA(caDir))
	require.NoError(t, tool.GenAPIClient(caDir, "admin", "127.0.0.1:"+strconv.Itoa(apiPort), admin))
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
	require.NoError(t, tool.GenNode(gen))
	userData, err := os.ReadFile(gen.Out)
	require.NoError(t, err)
	networkConfig, err := os.ReadFile(gen.NetworkConfig)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(caDir), "the authorities' keys are not kept")

	nd, err := node.Start(node.Spec{
		Config: cfg, Disk: copyBootDisk(t), WritableDisk: true, Dir: nodeDir,
		Mcast: fmt.Sprintf("230.78.0.1:%d", 20000+rand.IntN(40000)), NaruYAML: userData, NetworkConfig: networkConfig, //nolint:gosec // a port, not a secret
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		nd.Stop()
		t.Logf("console of %s:\n%s", nd.Name, nd.Console())
	})
	eventually(t, "the Node API answers an admin", func() bool { return ctl(admin, "version").Err == nil })
	return nd, admin
}

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
	dir := t.TempDir()
	bundle = filepath.Join(dir, release.BundleName(version))
	require.NoError(t, release.WriteBundle(bundle, version, filepath.Join(os.Getenv("NARU_UPGRADE_DIR"), kind, "bzImage")))
	b, err := os.ReadFile(bundle)
	require.NoError(t, err)
	sums = filepath.Join(dir, "SHA256SUMS")
	require.NoError(t, os.WriteFile(sums, []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(b), filepath.Base(bundle))), 0o600))
	return bundle, sums
}

// upgradeState is what `naructl upgrade --status` prints.
type upgradeState struct{ Version, BootID, Slot, Outcome, Reason string }

func statusOf(admin string) (upgradeState, error) {
	r := ctl(admin, "upgrade", "--status")
	if r.Err != nil {
		return upgradeState{}, r.Err
	}
	var s upgradeState
	for _, line := range strings.Split(r.Stdout, "\n") {
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
	before, err := statusOf(admin)
	require.NoError(t, err)
	require.Equal(t, want, before.Version)
	require.Equal(t, "default", before.Slot)
	require.NoError(t, ctl(admin, "apply", "-f", patchFile(t, idle("1m"))).Err)
	require.Equal(t, 1, revisionOf(t, admin))
	stored := storeOf(t, nd)
	bundle, sums := bundleFor(t, "next", next)

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	assert.Contains(t, r.Stdout, "the Node accepted the upgrade")
	awaitRestarting(t, nd, "the Node runs the upgraded kernel and has promoted it", func() bool {
		s, err := statusOf(admin)
		return err == nil && s.Version == next && s.Slot == "default" && strings.Contains(s.Outcome, "promoted")
	})
	after, err := statusOf(admin)
	require.NoError(t, err)
	assert.Equal(t, next, after.Version, "the version changed")
	assert.NotEqual(t, before.BootID, after.BootID, "an upgrade is a new boot")
	assert.Contains(t, after.Outcome, want+" -> "+next+", promoted")
	assert.Equal(t, 1, revisionOf(t, admin), "the store revision is the one before")
	assert.Equal(t, stored, storeOf(t, nd), "the file on the data disk reads back the same")
	assert.Contains(t, nd.Console(), "upgrade promoted")

	// Promotion is the rename of the slot: a power cycle boots the new kernel from the default slot.
	require.NoError(t, nd.Reboot())
	awaitRestarting(t, nd, "the new kernel boots from the default slot", func() bool {
		s, err := statusOf(admin)
		return err == nil && s.Version == next && s.Slot == "default" && s.BootID != after.BootID
	})
	assert.NotContains(t, nd.Console(), "upgrade rolled back")
}

func TestNode_UpgradeToAKernelThatRefusesIsRolledBackByTheNodeToTheVersionBefore(t *testing.T) {
	want := os.Getenv("NARU_VERSION")
	require.NotEmpty(t, want, "make test-qemu sets NARU_VERSION")
	refusing := want + "-refuse"
	nd, admin := startUpgradeNode(t)
	before, err := statusOf(admin)
	require.NoError(t, err)
	require.Equal(t, want, before.Version)
	require.NoError(t, ctl(admin, "apply", "-f", patchFile(t, idle("1m"))).Err)
	stored := storeOf(t, nd)
	bundle, sums := bundleFor(t, "refuse", refusing)

	r := ctl(admin, "upgrade", "--bundle", bundle, "--sums", sums)

	require.NoError(t, r.Err, r.refusal())
	// Nothing is done to the Node from here: the trial kernel cannot read its configuration, init
	// gives up on it and reboots, and the default slot boots again.
	awaitRestarting(t, nd, "the Node is back on its old version and says the upgrade rolled back", func() bool {
		s, err := statusOf(admin)
		return err == nil && strings.Contains(s.Outcome, "rolled back")
	})
	after, err := statusOf(admin)
	require.NoError(t, err)
	assert.Equal(t, want, after.Version, "the Node runs the version before")
	assert.Equal(t, "default", after.Slot)
	assert.NotEqual(t, before.BootID, after.BootID, "the trial and the boot after it are new boots")
	assert.Contains(t, after.Outcome, want+" -> "+refusing+", rolled back")
	assert.NotEmpty(t, after.Reason, "the outcome says why")
	assert.Contains(t, nd.Console(), "upgrade rolled back")
	assert.Equal(t, stored, storeOf(t, nd), "the data disk is as it was")
}
