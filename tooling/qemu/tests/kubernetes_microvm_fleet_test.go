//go:build qemu_real

package tests_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

// Two Kubernetes Nodes that are microVMs, each its own group of one member (ADR 0098), with the
// addresses the groups list (ADR 0099) and the Pod prefixes orch gives them, which reach the Nodes
// through the seed's meta-data and not through the user-data. Both Nodes are Ready in the cluster's
// API server. A Node is Ready only once the kubelet finds a CNI configuration, which init writes from
// spec.node.podPrefix, so a Ready Node shows the prefix came from the seed. The control is a third
// Node whose group has no network and so no Pod prefix: its seed names none, init writes no CNI file,
// and it stays NotReady beside the other two.
//
// Pods reaching each other is not checked here: a microVM Node has no registry to pull the sandbox
// and test images from (garamsh/naru#346).
func TestFleet_TwoKubernetesNodesThatAreMicroVMsOfOneMemberGroupsAreReadyWithTheirPodPrefixFromTheSeed(t *testing.T) {
	image, guestDir := os.Getenv("NARU_IMAGE_DIR"), os.Getenv("NARU_GUEST_DIR")
	if image == "" || guestDir == "" {
		t.Skip("NARU_IMAGE_DIR and NARU_GUEST_DIR name the files make image wrote")
	}
	_, err := exec.LookPath("kubectl")
	require.NoError(t, err, "kubectl is a precondition of make test-qemu")
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1, vmMiB: 3*node.KubeNodeMemoryMiB + 1024})
	f.startOrch()
	h := f.hosts()[0]
	f.enrollAndAdmit("eu-1")
	operator := f.operator("ops-1", "operator")
	api := f.hostOfNode(h).Addr
	call := func(words ...string) result {
		args := append(append([]string{}, words...), "--identity", operator.file, "--passphrase-file", f.pass, "--host-ca", f.c.HostCA, "--endpoint", api)
		return nrun(args...)
	}

	assets := t.TempDir()
	require.NoError(t, release.WriteBootSet(assets, "qemu-kube",
		filepath.Join(guestDir, "boot-sets", release.NodeBootSet, "vmlinux"), filepath.Join(image, "node-initrd.cpio.gz")))
	const set = "naru-node-qemu-kube"
	f.must(call("host", "boot-set", "add", filepath.Join(assets, set+".json")), "add the boot set")
	var host struct {
		Mvmd struct {
			MachinePrefixes []string `json:"machinePrefixes"`
		} `json:"mvmd"`
	}
	eventually(t, "the host lists the boot set and its Machine prefix", func() bool {
		r := call("host")
		return r.Err == nil && strings.Contains(r.Stdout, set) && json.Unmarshal([]byte(r.Stdout), &host) == nil && len(host.Mvmd.MachinePrefixes) > 0
	})
	machine := netip.MustParsePrefix(host.Mvmd.MachinePrefixes[0])
	at := func(n int) netip.Addr {
		a := machine.Masked().Addr()
		for range n {
			a = a.Next()
		}
		return a
	}
	cpAddr, wAddr, bareAddr := at(10), at(11), at(12)

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	require.NoError(t, tool.GenCA(ca))
	require.NoError(t, tool.GenKubernetes(ca, cpAddr.String(), ""))
	userData := func(name, role string, addr netip.Addr) string {
		out := filepath.Join(dir, name+".yaml")
		require.NoError(t, tool.GenKubernetesNode(naructl.KubernetesNode{
			CA: ca, Name: name + "-1", Role: role, Address: netip.PrefixFrom(addr, machine.Bits()).String(), Endpoint: cpAddr.String(),
			PodNetwork: node.KubePodNetwork, PodPrefixFromSeed: true, Out: out,
		}))
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		q, err := json.Marshal(string(b))
		require.NoError(t, err)
		return string(q)
	}
	group := func(name, role string, addr netip.Addr, network string) string {
		policy := `  policy:
    ingress:
      - {prefix: 0.0.0.0/0, protocol: tcp, ports: [6443, 10250, 2379, 2380]}
`
		net := ""
		if network != "" {
			net = fmt.Sprintf("  network: %s\n  podPrefixBits: 24\n", network)
		}
		return f.file(name+".yaml", fmt.Sprintf(`name: %s
count: 1
region: eu-1
template:
  form: os
  cpus: 2
  memoryMiB: %d
  nodeVersion: %s
  addresses: [%s]
  disks:
    - {name: data, sizeBytes: 4294967296}
  userData: %s
%s%srollout: {maxUnavailable: 0, maxSurge: 0}
`, name, node.KubeNodeMemoryMiB, set, addr, userData(name, role, addr), policy, net))
	}
	f.must(f.orch("network", "apply", "-f", f.networkFile("kube", node.KubePodNetwork)), "declare the Pod network")
	for _, g := range []struct {
		name, role string
		addr       netip.Addr
		network    string
	}{{"cp1", "control-plane", cpAddr, "kube"}, {"w1", "worker", wAddr, "kube"}, {"bare", "worker", bareAddr, ""}} {
		f.must(f.orch("group", "apply", "-f", group(g.name, g.role, g.addr, g.network)), "apply "+g.name)
	}
	eventually(t, "all three members run", func() bool {
		for _, n := range []string{"cp1", "w1", "bare"} {
			if g, ok := f.group(1, n); !ok || g.count("running") != 1 {
				return false
			}
		}
		return true
	})

	kube, stop, err := f.hostOfNode(h).RouteTo(t.Context(), net.JoinHostPort(cpAddr.String(), "6443"))
	require.NoError(t, err)
	defer stop()
	var last string
	defer func() { t.Logf("the last kubectl answer:\n%s", last) }()
	states := func() map[string]string {
		out, err := exec.Command("kubectl", "--kubeconfig", filepath.Join(ca, "admin.conf"), "--server", "https://"+kube,
			"--tls-server-name", cpAddr.String(), "--request-timeout=20s", "get", "nodes", "--no-headers").CombinedOutput()
		last = string(out)
		got := map[string]string{}
		if err != nil {
			return got
		}
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) >= 2 {
				got[f[0]] = f[1]
			}
		}
		return got
	}
	require.Eventually(t, func() bool {
		s := states()
		return s["cp1-1"] == "Ready" && s["w1-1"] == "Ready" && s["bare-1"] != ""
	}, 12*time.Minute, 5*time.Second, "the two Nodes with a Pod prefix from the seed are Ready, and the third has joined")
	assert.Equal(t, "NotReady", states()["bare-1"], "the control: a Node whose seed names no Pod prefix has no CNI configuration and is not Ready")
}
