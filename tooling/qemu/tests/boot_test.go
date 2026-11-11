//go:build qemu_real

// Package tests boots naru's UEFI disk image under QEMU. It needs the image built
// (make image), the disk (make disk), the guest inputs (make guest-inputs), /dev/kvm with nested
// virtualization, and the host's mke2fs; `make test-qemu` runs it.
package tests_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/judge"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

const bootTimeout = 3 * time.Minute

// inputs is the staged tree every Node of every test carries.
var inputs fixture.Inputs

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	root, image, guest := os.Getenv("NARU_ROOT"), os.Getenv("NARU_IMAGE_DIR"), os.Getenv("NARU_GUEST_DIR")
	if root == "" || image == "" || guest == "" {
		os.Stderr.WriteString("NARU_ROOT, NARU_IMAGE_DIR and NARU_GUEST_DIR are required; make test-qemu sets them\n")
		return 1
	}
	dir, err := os.MkdirTemp("", "naru15-inputs-")
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	inputs, err = fixture.Stage(fixture.Sources{Root: root, ImageDir: image, GuestDir: guest}, dir)
	if err != nil {
		os.Stderr.WriteString("stage the inputs: " + err.Error() + "\n")
		return 1
	}
	return m.Run()
}

// boot starts n Nodes on one network that boot the image's UEFI disk, naru.img, under OVMF, each
// with its own NoCloud drive; probeOn is the one that runs the probe, or 0.
func boot(t *testing.T, n, probeOn int) *node.Cluster {
	t.Helper()
	c, err := node.StartCluster(node.ClusterSpec{
		N: n, Disk: filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), Dir: t.TempDir(), Inputs: inputs, ProbeOn: probeOn,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Stop()
		for _, nd := range c.Nodes {
			t.Logf("console of %s:\n%s", nd.Name, nd.Console())
		}
	})
	return c
}

// TestNode_UEFIDiskBootsToInitAndStartsItsServices boots two Nodes from naru.img under OVMF, with
// no -kernel, -initrd or -append, and checks on each console that init read its NoCloud drive,
// configured the network and started mvmd and the stand-in.
func TestNode_UEFIDiskBootsToInitAndStartsItsServices(t *testing.T) {
	c := boot(t, 2, 0)

	for _, nd := range c.Nodes {
		expectAll(t, nd, judge.Boot(nd.Name))
		expectQEMUNameservers(t, nd)
	}
}

// TestNode_ABrokenNetworkConfigRefusesTheBoot gives a Node a network-config whose address is not
// one, and checks that init refuses the boot before it starts any service.
func TestNode_ABrokenNetworkConfigRefusesTheBoot(t *testing.T) {
	cfg := node.Config{Name: "node-1", Index: 1, Total: 2, ControlPlane: true, Inputs: inputs}
	broken := strings.Replace(string(cfg.NetworkConfig()), "address: '10.78.0.1'", "address: 'not-an-address'", 1)
	require.NotEqual(t, string(cfg.NetworkConfig()), broken, "the network-config was changed")
	n, err := node.Start(node.Spec{
		Config: cfg, Disk: filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), Dir: t.TempDir(),
		Mcast: "230.78.0.1:25000", NetworkConfig: []byte(broken),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		n.Stop()
		t.Logf("console of %s:\n%s", n.Name, n.Console())
	})

	require.NoError(t, n.WaitFor(context.Background(), bootTimeout, "boot refused; no further service will start"))
	assert.Error(t, n.WaitFor(context.Background(), time.Millisecond, "started", "service="), "no service started")
}

// TestNode_TwoNodesRunTheirServicesAndReachEachOther boots two Nodes on one
// network and checks, on each, that init started every configured service,
// that mvmd answers on its Unix socket, and that the stand-in dialled the
// stand-in of the other Node across the network.
func TestNode_TwoNodesRunTheirServicesAndReachEachOther(t *testing.T) {
	c := boot(t, 2, 0)

	for _, nd := range c.Nodes {
		expectAll(t, nd, judge.Services(nd.Name))
		expectQEMUNameservers(t, nd)
	}
}

// TestNode_ControlPlaneServicesStartOnlyOnTheControlPlaneNode checks both
// Nodes: node-1 has spec.controlPlane true and starts the control service,
// node-2 has it false and init skips the service.
func TestNode_ControlPlaneServicesStartOnlyOnTheControlPlaneNode(t *testing.T) {
	c := boot(t, 2, 0)
	ctx := context.Background()
	first, second := c.Nodes[0], c.Nodes[1]

	expectAll(t, first, judge.ControlPlane(true))
	expectAll(t, second, judge.ControlPlane(false))
	require.NoError(t, second.WaitFor(ctx, bootTimeout, "started", "service=standin"))
}

// TestNode_MachineSurvivesMvmdRestart has the probe start a Machine, kill mvmd
// and check that init restarts it, that the supervisor is the same live
// process, and that mvmd reports the Machine running after it reattaches. The
// host reads the probe's PROBE lines from the console.
func TestNode_MachineSurvivesMvmdRestart(t *testing.T) {
	c := boot(t, 2, 1)

	expectAll(t, c.Nodes[0], judge.Probe())
}

// TestNode_AConsoleReaderThatAttachesLateSeesTheWholeLog boots a Node and waits for init's first
// re-print: a block, between its numbered markers, that holds the lines init printed at boot, so
// a reader that attached after them still has them.
func TestNode_AConsoleReaderThatAttachesLateSeesTheWholeLog(t *testing.T) {
	c := boot(t, 2, 0)
	nd := c.Nodes[0]

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "NARU-LOG-END seq=1"), "init re-prints its log")

	lines := strings.Split(nd.Console(), "\n")
	begin, end := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "NARU-LOG-BEGIN seq=1 ") && begin < 0 {
			begin = i
		}
		if strings.HasPrefix(l, "NARU-LOG-END seq=1") && end < 0 {
			end = i
		}
	}
	require.True(t, begin >= 0 && end > begin, "the first block has both its markers")
	block := strings.Join(lines[begin:end], "\n")
	assert.True(t, judge.HasLine(block, []string{"configuration accepted", "node=naru-node-1"}), "the block holds init's first lines")
	assert.True(t, judge.HasLine(block, []string{"started", "service=mvmd"}), "and the lines that came after")
}

// expectAll waits for the Node's console to meet each expectation: to show the line, or, for an
// absent one, to show no such line once the rest has been seen. These are the expectations
// `make judge-serial` checks on the serial logs of a real deployment.
func expectAll(t *testing.T, nd *node.Node, want []judge.Expect) {
	t.Helper()
	for _, e := range want {
		if e.Absent {
			continue
		}
		require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, e.Fragments...), "%s: %s: %q", nd.Name, e.What, e.Fragments)
	}
	for _, e := range want {
		if e.Absent {
			assert.True(t, e.Met(nd.Console()), "%s: %s: %q", nd.Name, e.What, e.Fragments)
		}
	}
}

// expectQEMUNameservers waits for init to have configured exactly one nameserver: the count of the
// QEMU network-config, which is a fact of this network and not of every Node the judge reads.
func expectQEMUNameservers(t *testing.T, nd *node.Node) {
	t.Helper()
	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "network configured", "interfaces=1", "nameservers=1"), "%s: init configured one nameserver", nd.Name)
}
