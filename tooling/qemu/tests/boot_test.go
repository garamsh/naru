//go:build qemu_real

// Package tests boots naru's image under QEMU. It needs the image built
// (make image), the guest inputs (make guest-inputs), /dev/kvm with nested
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

// boot starts n Nodes on one network; probeOn is the one that runs the probe, or 0.
func boot(t *testing.T, n, probeOn int) *node.Cluster {
	t.Helper()
	image := os.Getenv("NARU_IMAGE_DIR")
	c, err := node.StartCluster(node.ClusterSpec{
		N: n, Kernel: filepath.Join(image, "bzImage"), Initrd: filepath.Join(image, "initramfs.cpio.gz"),
		Dir: t.TempDir(), Inputs: inputs, ProbeOn: probeOn,
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

// bootDisk starts n Nodes on one network that boot the image's UEFI disk, naru.img, under OVMF.
func bootDisk(t *testing.T, n int) *node.Cluster {
	t.Helper()
	c, err := node.StartCluster(node.ClusterSpec{
		N: n, Disk: filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), Dir: t.TempDir(), Inputs: inputs,
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
// no -kernel, -initrd or -append, and checks on each console that init accepted the
// configuration and started mvmd and the stand-in. The Nodes have no addresses until the
// configuration carries them, so the cross-network dial is not asserted.
func TestNode_UEFIDiskBootsToInitAndStartsItsServices(t *testing.T) {
	c := bootDisk(t, 2)
	ctx := context.Background()

	for _, nd := range c.Nodes {
		for _, step := range [][]string{
			{"configuration accepted", "node=naru5-" + nd.Name},
			{"root remounted read-only"},
			{"data disk is mounted", "dir=/var/lib/mvmd", "fstype=ext4"},
			{"started", "service=mvmd"},
			{"started", "service=standin"},
		} {
			require.NoError(t, nd.WaitFor(ctx, bootTimeout, step...), "%s: %q", nd.Name, step)
		}
	}
}

// TestNode_TwoNodesRunTheirServicesAndReachEachOther boots two Nodes on one
// network and checks, on each, that init started every configured service,
// that mvmd answers on its Unix socket, and that the stand-in dialled the
// stand-in of the other Node across the network.
func TestNode_TwoNodesRunTheirServicesAndReachEachOther(t *testing.T) {
	c := boot(t, 2, 0)
	ctx := context.Background()

	for _, nd := range c.Nodes {
		for _, step := range [][]string{
			{"configuration accepted", "node=naru5-" + nd.Name},
			{"root remounted read-only"},
			{"started", "service=mvmd"},
			{"started", "service=registry"},
			{"started", "service=standin"},
			{"data disk is mounted", "dir=/var/lib/mvmd", "fstype=ext4"},
			{"socket accepts connections", "path=/run/mvmd.sock"},
			{"peer accepts connections"},
		} {
			require.NoError(t, nd.WaitFor(ctx, bootTimeout, step...), "%s: %q", nd.Name, step)
		}
	}
}

// TestNode_ControlPlaneServicesStartOnlyOnTheControlPlaneNode checks both
// Nodes: node-1 has spec.controlPlane true and starts the control service,
// node-2 has it false and init skips the service.
func TestNode_ControlPlaneServicesStartOnlyOnTheControlPlaneNode(t *testing.T) {
	c := boot(t, 2, 0)
	ctx := context.Background()
	first, second := c.Nodes[0], c.Nodes[1]

	require.NoError(t, first.WaitFor(ctx, bootTimeout, "started", "service=control"), "the control-plane Node starts the service")
	require.NoError(t, second.WaitFor(ctx, bootTimeout, "skipping a control-plane service", "service=control"), "the other Node skips it")
	require.NoError(t, second.WaitFor(ctx, bootTimeout, "started", "service=standin"))
	assert.Error(t, second.WaitFor(ctx, time.Millisecond, "started", "service=control"), "the control service never started there")
	assert.Error(t, first.WaitFor(ctx, time.Millisecond, "skipping a control-plane service"), "the control-plane Node skips nothing")
}

// TestNode_MachineSurvivesMvmdRestart has the probe start a Machine, kill mvmd
// and check that init restarts it, that the supervisor is the same live
// process, and that mvmd reports the Machine running after it reattaches. The
// host reads the probe's PROBE lines from the console.
func TestNode_MachineSurvivesMvmdRestart(t *testing.T) {
	c := boot(t, 2, 1)
	nd := c.Nodes[0]
	ctx := context.Background()

	for _, check := range []string{
		"mvmd-ready", "fencing-armed", "image-pulled", "machine-started", "machine-running", "supervisor-found", "mvmd-found",
		"mvmd-killed", "mvmd-restarted", "supervisor-alive", "supervisor-same-pid", "machine-still-running",
	} {
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "PROBE "+check+" ok"), "the probe reports %s", check)
	}
	require.NoError(t, nd.WaitFor(ctx, bootTimeout, "PROBE finished"))
	for _, line := range strings.Split(nd.Console(), "\n") {
		assert.False(t, strings.HasPrefix(line, "PROBE ") && strings.Contains(line, " fail "), line)
	}
	require.NoError(t, nd.WaitFor(ctx, time.Second, "restarting", "service=mvmd"), "init restarted mvmd")
}
