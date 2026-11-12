//go:build qemu_real

// Package tests boots naru's UEFI disk image under QEMU. It needs the image built
// (make image, which includes the guest files), the disk (make disk), /dev/kvm with nested
// virtualization, and the host's mkfs.ext4 (for the data disk); `make test-qemu` runs it.
package tests_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/gaoncli"
	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/judge"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

const bootTimeout = 3 * time.Minute

// inputs is the staged tree every Node of every test carries.
var inputs fixture.Inputs

// tool is naructl built from the checkout, which writes the user-data of every gaon Node.
var tool naructl.Tool

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
	bin, err := os.MkdirTemp("", "naru36-naructl-")
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		return 1
	}
	defer func() { _ = os.RemoveAll(bin) }()
	tool = naructl.Tool{Bin: filepath.Join(bin, "naructl")}
	if err := naructl.Build(root, tool.Bin); err != nil {
		os.Stderr.WriteString("build naructl: " + err.Error() + "\n")
		return 1
	}
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
	return start(t, node.ClusterSpec{N: n, ProbeOn: probeOn})
}

// start boots the cluster spec describes, on the image's disk, and stops it when the test ends.
func start(t *testing.T, spec node.ClusterSpec) *node.Cluster {
	t.Helper()
	spec.Disk, spec.Dir, spec.Inputs, spec.Naructl = filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), t.TempDir(), inputs, tool
	c, err := node.StartCluster(spec)
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

// TestNode_GaonRegistersEachNodeKeepsItsLeaseAndSurvivesTheControlPlaneNodeRebooting boots two Nodes
// that run gaon and talks to the API server from the host, through the host forward, with the test
// host's client certificate: both Nodes register and are Ready, each Node's Lease is renewed, and a
// resource written before node-1 loses power is served after it boots again, from etcd's data on
// the data disk. Every pair of every Node, node-1's included, is an operator-supplied file of
// spec.files, as in v0.2.0: this is the test of that path, with no spec.pki.
func TestNode_GaonRegistersEachNodeKeepsItsLeaseAndSurvivesTheControlPlaneNodeRebooting(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 2, Gaon: true})
	api, err := hostapi.New(c.HostAddr, c.Client)
	require.NoError(t, err)
	ctx := context.Background()
	first, second := c.Nodes[0], c.Nodes[1]

	// The control-plane Node has the second NIC, so it configures two interfaces; the other, one.
	require.NoError(t, first.WaitFor(ctx, bootTimeout, "network configured", "interfaces=2"))
	require.NoError(t, second.WaitFor(ctx, bootTimeout, "network configured", "interfaces=1"))
	for _, nd := range c.Nodes {
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=gaon-node-agent"), nd.Name)
	}

	for _, nd := range c.Nodes {
		eventually(t, nd.Name+" is registered and Ready", func() bool {
			n, err := api.Node(ctx, nd.Name)
			return err == nil && n.Ready()
		})
	}

	for _, nd := range c.Nodes {
		var before time.Time
		eventually(t, nd.Name+" has a Lease", func() bool {
			l, err := api.Lease(ctx, nd.Name)
			before = l.Spec.RenewTime
			return err == nil && !before.IsZero()
		})
		eventually(t, nd.Name+"'s Lease is renewed", func() bool {
			l, err := api.Lease(ctx, nd.Name)
			return err == nil && l.Spec.RenewTime.After(before)
		})
	}

	require.NoError(t, api.CreateNode(ctx, "marker"))
	_, err = api.Node(ctx, "marker")
	require.NoError(t, err, "the marker is served before the reboot")

	require.NoError(t, first.Reboot())
	require.NoError(t, first.WaitFor(ctx, bootTimeout, "started", "service=etcd"), "node-1 booted again")
	eventually(t, "the marker is served after node-1 rebooted", func() bool {
		_, err := api.Node(ctx, "marker")
		return err == nil
	})
	eventually(t, "node-1 is Ready again", func() bool {
		n, err := api.Node(ctx, "node-1")
		return err == nil && n.Ready()
	})
}

// TestNode_GaonRunsFromSpecPKIOnTheControlPlaneNodeWhichIssuesItsOwnCertificates boots two Nodes that
// run gaon where node-1's configuration carries the CA and its key in spec.pki and no pair as a file,
// and node-2 carries an operator agent pair in spec.files (ADR 0013). The host reaches node-1's API
// server through the host forward, verifying the certificate init issued against the CA and its
// names (127.0.0.1 is the host forward's), and both Nodes register and are Ready: node-1's agent
// with the pair init issued, node-2's with the pair it was given. The console of node-1 shows init
// issuing each pair before the first service starts, and no key.
func TestNode_GaonRunsFromSpecPKIOnTheControlPlaneNodeWhichIssuesItsOwnCertificates(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 2, Gaon: true, BootPKI: true})
	api, err := hostapi.New(c.HostAddr, c.Client)
	require.NoError(t, err)
	ctx := context.Background()
	first, second := c.Nodes[0], c.Nodes[1]

	for _, nd := range c.Nodes {
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=gaon-node-agent"), nd.Name)
	}
	for _, nd := range c.Nodes {
		eventually(t, nd.Name+" is registered and Ready", func() bool {
			n, err := api.Node(ctx, nd.Name)
			return err == nil && n.Ready()
		})
	}

	console := judge.Log{Text: first.Console()}
	assert.Empty(t, judge.Check(first.Name, console, judge.Issuance(judge.GaonIssued...)), "init issued every pair before it started a service")
	assert.False(t, judge.HasLine(console.Text, []string{"PRIVATE KEY"}), "no key is logged")
	assert.False(t, judge.HasLine(second.Console(), []string{"authority written"}), "node-2 holds no CA and issues nothing")
}

// rootMounted is the line of the guest's console that says it mounted its root disk, vdb, as ext4.
const rootMounted = "EXT4-fs (vdb): mounted filesystem"

// machineManifest is a Machine as a user writes it for gaon's CLI: the registry image by digest,
// the Always run strategy, a 256 MiB root disk (mvmd refuses a Machine with none) and the requests
// the scheduler weighs.
const machineManifest = `apiVersion: compute.gaon.garam.sh/v1alpha1
kind: Machine
metadata:
  name: hello
spec:
  image: %s@%s
  runStrategy: Always
  rootDiskSize: 256Mi
  resources:
    requests:
      cpu: 500m
      memory: 256Mi
`

// TestNode_AMachineAppliedWithTheGaonCLIIsBoundRunsOnTheGuestKernelFromTheTestRegistryOnAMkfsRootDisk
// boots two Nodes that run gaon, admits both with gaon's CLI from the host, applies a Machine through
// node-1's API server with the same CLI, and waits for the scheduler to bind it and for it to be
// Running. On the Node it is bound to, the Node's registry served the image's manifest, the Machine's
// console (the vmm.log mvmd keeps for it) shows the guest kernel's banner and the guest mounting its
// root disk as ext4, and that root disk is the ext4 file system, with that UUID, that mvmd made with
// /bin/mkfs.ext4 (its disk.mkfs).
func TestNode_AMachineAppliedWithTheGaonCLIIsBoundRunsOnTheGuestKernelFromTheTestRegistryOnAMkfsRootDisk(t *testing.T) {
	cliBin := filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "gaon", "gaon")
	require.FileExists(t, cliBin, "make image builds gaon's CLI from the pin")
	c := start(t, node.ClusterSpec{N: 2, Gaon: true})
	api, err := hostapi.New(c.HostAddr, c.Client)
	require.NoError(t, err)
	cli := gaoncli.New(cliBin, c.HostAddr, c.Client)
	ctx := context.Background()
	work := t.TempDir()

	for _, nd := range c.Nodes {
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=registry"), nd.Name)
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=gaon-node-agent"), nd.Name)
	}
	for _, nd := range c.Nodes {
		eventually(t, nd.Name+" is registered and Ready", func() bool {
			n, err := api.Node(ctx, nd.Name)
			return err == nil && n.Ready()
		})
		out, err := cli.Apply(ctx, work, fmt.Sprintf("apiVersion: compute.gaon.garam.sh/v1alpha1\nkind: Node\nmetadata:\n  name: %s\nspec:\n  admitted: true\n", nd.Name))
		require.NoError(t, err)
		t.Logf("gaon apply Node %s: %s", nd.Name, strings.TrimSpace(out))
		eventually(t, nd.Name+" is Admitted", func() bool {
			n, err := api.Node(ctx, nd.Name)
			return err == nil && n.Status.State == "Admitted"
		})
	}

	out, err := cli.Apply(ctx, work, fmt.Sprintf(machineManifest, inputs.Image.Repository, inputs.Image.Digest))
	require.NoError(t, err)
	t.Logf("gaon apply Machine: %s", strings.TrimSpace(out))

	// snapshot copies the bound Node's data disk as it is now; the copy is replaced at each call.
	snapshot := func(bound *node.Node) (*node.DataSnapshot, error) {
		img := filepath.Join(work, "data-"+bound.Name+".img")
		_ = os.Remove(img)
		return bound.SnapshotData(img)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		got, _ := cli.Run(ctx, "get", "machine", "hello")
		t.Logf("gaon get machine hello:\n%s", got)
		for _, nd := range c.Nodes {
			sn, err := snapshot(nd)
			if err != nil {
				t.Logf("%s: snapshot: %v", nd.Name, err)
				continue
			}
			var walk func(dir string, depth int)
			walk = func(dir string, depth int) {
				names, err := sn.List(dir)
				t.Logf("%s %s: %v %v", nd.Name, dir, names, err)
				if depth == 0 {
					return
				}
				for _, n := range names {
					if !strings.Contains(n, ".") {
						walk(dir+"/"+n, depth-1)
					}
				}
			}
			walk(node.DataMount+"/mvmd", 4)
			for _, f := range []string{"machines/hello-e1/run/vmm.log"} {
				b, err := sn.Read(node.DataMount + "/mvmd/" + f)
				t.Logf("%s %s (%v):\n%s", nd.Name, f, err, b)
			}
		}
	})

	var m hostapi.Machine
	eventually(t, "the Machine is bound and Running", func() bool {
		m, err = api.Machine(ctx, "hello")
		return err == nil && m.Running()
	})
	var bound *node.Node
	for _, nd := range c.Nodes {
		if nd.Name == m.Spec.NodeName {
			bound = nd
		}
	}
	require.NotNil(t, bound, "the Machine is bound to one of the two Nodes: %q", m.Spec.NodeName)
	t.Logf("the scheduler bound the Machine to %s at epoch %d", bound.Name, m.Spec.BindingEpoch)

	// The image comes from the test registry of the bound Node: its console shows the manifest served.
	require.NoError(t, bound.WaitFor(ctx, bootTimeout, "served", inputs.Image.Digest), "the registry served the manifest")

	// The Machine's console: mvmd writes the guest's serial output to the Machine's vmm.log, which
	// the host reads from a copy of the Node's data disk.
	var vmm string
	var snap *node.DataSnapshot
	eventually(t, "the Machine's console shows the guest mounting its root disk", func() bool {
		snap, err = snapshot(bound)
		if err != nil {
			return false
		}
		ids, err := snap.List(node.DataMount + "/mvmd/machines")
		if err != nil {
			return false
		}
		for _, id := range ids {
			b, err := snap.Read(node.DataMount + "/mvmd/machines/" + id + "/run/vmm.log")
			if err == nil && strings.Contains(string(b), rootMounted) {
				vmm = string(b)
				return true
			}
		}
		return false
	})
	t.Logf("the Machine's console (vmm.log):\n%s", vmm)
	mounted := regexp.MustCompile(`EXT4-fs \(vdb\): mounted filesystem ([0-9a-f-]{36})`).FindStringSubmatch(vmm)
	require.NotNil(t, mounted, "the guest mounted its root disk, vdb, as ext4")
	vmlinux, err := os.ReadFile(filepath.Join(os.Getenv("NARU_GUEST_DIR"), "kernels", "naru", "vmlinux"))
	require.NoError(t, err)
	banner := regexp.MustCompile(`Linux version [^\n\x00]+`).Find(vmlinux)
	require.NotEmpty(t, banner, "the guest kernel carries its banner")
	assert.Contains(t, vmm, string(banner), "the Machine booted the image's guest kernel")

	// The root disk is ext4, made by the mkfs mvmd runs.
	disks, err := snap.List(node.DataMount + "/mvmd/disks")
	require.NoError(t, err)
	var disk string
	for _, n := range disks {
		if strings.HasSuffix(n, "-root") {
			disk = n
		}
	}
	require.NotEmpty(t, disk, "mvmd made the Machine's root disk: %v", disks)
	img := filepath.Join(work, "root.img")
	require.NoError(t, snap.Extract(node.DataMount+"/mvmd/disks/"+disk, img))
	sb, err := node.Superblock(img)
	require.NoError(t, err)
	assert.Contains(t, sb, "Filesystem magic number:  0xEF53", "the root disk is an ext4 file system")
	assert.Contains(t, sb, "Filesystem UUID:          "+mounted[1], "it is the file system the guest mounted, which mvmd made with -U")
	t.Logf("root disk %s:\n%s", disk, sb)
}

// eventually polls cond until it holds, failing the test when bootTimeout passes first. Each poll
// is a read, so the wait is on the condition and not on a fixed delay.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, bootTimeout, time.Second, what)
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
