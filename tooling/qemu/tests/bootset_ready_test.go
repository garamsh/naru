//go:build qemu_real

package tests_test

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/hostcli"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/release"
	"github.com/garamsh/naru/tooling/qemu/internal/seed"
)

// Criterion "host: the boot set added through naructl host boot-set add, and a Node microVM from it
// that reports Ready and answers the Node API's version". The test host reaches the microVM's address
// through the harness's route (docs/architecture/harness.md): node-1's naru-mvmroute, which dials the
// microVM from inside node-1's network.
//
// mvmd gives a microVM its address, so the seed's address is known only after a microVM exists. The
// test creates the microVM with a plain seed, reads the address mvmd gave it, deletes it, and creates
// it again with the seed written for that address, until the second creation is given the same one.
func TestNode_HostBootSetMicroVMReportsReadyToItsAPIServerAndAnswersTheNodeAPIVersionThroughTheHarnessRoute(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is not installed on the test host")
	}
	c := start(t, node.ClusterSpec{N: 1, Gaon: true, NodeAPI: true, Host: true, MemoryMiB: 2*node.KubeNodeMemoryMiB + 1024})
	require.NotNil(t, c.Host)
	eventually(t, "the host API answers a reader", func() bool { return hctl(c.Host.Reader, []string{"host"}).Err == nil })
	nd := c.Nodes[0]
	prov := c.Host.Provisioner
	require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, "started", "service=naru-hostd"))
	image, guestDir := os.Getenv("NARU_IMAGE_DIR"), os.Getenv("NARU_GUEST_DIR")

	assets := t.TempDir()
	require.NoError(t, release.WriteBootSet(assets, "qemu-ready",
		filepath.Join(guestDir, "boot-sets", release.NodeBootSet, "vmlinux"), filepath.Join(image, "node-initrd.cpio.gz")))
	const set = "naru-node-qemu-ready"
	added := hctl(prov, []string{"host", "boot-set", "add"}, filepath.Join(assets, set+".json"))
	require.NoError(t, added.Err, added.refusal())
	assert.Contains(t, added.Stdout, "added boot set "+set)

	dir := t.TempDir()
	// A microVM's console is its vmm.log in mvmd's data, which the host reads from a copy of the
	// Node's data disk; a failed run shows it, as the Machine test does.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		sn, err := nd.SnapshotData(filepath.Join(dir, "data.img"))
		if err != nil {
			t.Logf("snapshot of the data disk: %v", err)
			return
		}
		ids, _ := sn.List(node.DataMount + "/mvmd/machines")
		for _, id := range ids {
			b, err := sn.Read(node.DataMount + "/mvmd/machines/" + id + "/run/vmm.log")
			t.Logf("microVM %s console (%v):\n%s", id, err, b)
		}
	})
	ca := filepath.Join(dir, "ca")
	require.NoError(t, tool.GenCA(ca))
	require.NoError(t, tool.GenAPICA(ca))
	client, err := hostcli.New(prov)
	require.NoError(t, err)
	ctx := context.Background()

	const name = "kcp"
	// A microVM admits no traffic it has no rule for, so the request allows the API server's port and
	// the Node API's, which the harness's route reaches from node-1.
	create := func(seedFile string) {
		t.Helper()
		user, err := os.ReadFile(seedFile)
		require.NoError(t, err)
		allow := func(port string) hostapi.Rule {
			n, err := strconv.ParseUint(port, 10, 32)
			require.NoError(t, err)
			return hostapi.Rule{Prefix: "0.0.0.0/0", Protocol: "tcp", Ports: []hostapi.PortRange{{First: uint32(n), Last: uint32(n)}}}
		}
		request, err := json.Marshal(hostapi.CreateRequest{
			Name: name, CPU: 2, Memory: 4 << 30, BootSet: set, Seed: user, DataDisk: 2 << 30, PodPrefix: node.KubePodPrefix(1),
			NetworkPolicy: hostapi.Policy{Ingress: []hostapi.Rule{allow("6443"), allow(node.NodeAPIPort)}},
		})
		require.NoError(t, err)
		file := filepath.Join(dir, "microvm.json")
		require.NoError(t, os.WriteFile(file, request, 0o600))
		made := hctl(prov, []string{"microvm", "create"}, "-f", file)
		require.NoError(t, made.Err, made.refusal())
	}
	addressOf := func() netip.Addr {
		t.Helper()
		var addr netip.Addr
		eventually(t, "the microVM has an address", func() bool {
			vm, err := client.MicroVM(ctx, name)
			if err != nil || len(vm.Addresses) == 0 {
				return false
			}
			s, _, _ := strings.Cut(vm.Addresses[0], "/")
			addr, err = netip.ParseAddr(s)
			return err == nil
		})
		return addr
	}
	plain := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(plain, []byte("#cloud-config\n"), 0o600))
	var addr netip.Addr
	for attempt := 0; ; attempt++ {
		require.Less(t, attempt, 3, "mvmd did not give the microVM the same address twice")
		create(plain)
		guess := addressOf()
		require.NoError(t, client.DeleteMicroVM(ctx, name))
		eventually(t, "the probe microVM is gone", func() bool { return machines(nd) == "none" })

		require.NoError(t, tool.GenKubernetes(ca, guess.String(), ""))
		user := filepath.Join(dir, "user-data")
		require.NoError(t, tool.GenKubernetesNode(naructl.KubernetesNode{
			CA: ca, Name: name, Role: "control-plane", Address: guess.String() + "/24", Endpoint: guess.String(),
			PodNetwork: node.KubePodNetwork, PodPrefix: node.KubePodPrefix(1), Out: user,
		}))
		require.NoError(t, seed.AddNodeAPI(user, ca, net.IP(guess.AsSlice()), net.ParseIP("127.0.0.1")))
		create(user)
		if addr = addressOf(); addr == guess {
			break
		}
		require.NoError(t, client.DeleteMicroVM(ctx, name))
		eventually(t, "the mismatched microVM is gone", func() bool { return machines(nd) == "none" })
	}
	t.Logf("the Node microVM has the address %s", addr)

	// The API server lists the microVM's Node Ready, through the route.
	kube, stopKube, err := c.Host.RouteTo(ctx, net.JoinHostPort(addr.String(), "6443"))
	require.NoError(t, err)
	defer stopKube()
	var last string
	defer func() { t.Logf("the last kubectl answer: %s", last) }()
	require.Eventually(t, func() bool {
		out, err := exec.Command("kubectl", "--kubeconfig", filepath.Join(ca, "admin.conf"), "--server", "https://"+kube,
			"--tls-server-name", addr.String(), "--request-timeout=20s", "get", "nodes", "--no-headers").CombinedOutput()
		last = string(out)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == name && f[1] == "Ready" {
				return true
			}
		}
		return false
	}, 10*time.Minute, 5*time.Second, "the API server of the microVM lists %s Ready", name)

	// The Node API of the microVM answers version.
	api, stopAPI, err := c.Host.RouteTo(ctx, net.JoinHostPort(addr.String(), node.NodeAPIPort))
	require.NoError(t, err)
	defer stopAPI()
	file := filepath.Join(dir, "admin.yaml")
	require.NoError(t, tool.GenAPIClient(ca, "admin", api, file))
	version, bootID := versionOf(t, file)
	assert.NotEmpty(t, version)
	assert.NotEmpty(t, bootID)
	t.Logf("the microVM's Node API answered version %s, boot ID %s", version, bootID)
}
