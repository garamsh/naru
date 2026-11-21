//go:build qemu_real

package tests_test

import (
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/hostcli"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The exposure scenario (#275): a client on the LAN reaches a microVM of a network through a leased
// address, forwarded and through the PROXY protocol v2 listener of the host, and cannot once the lease
// is deleted, moved to the other host or not renewed. The client is naru-lanclient on every Node, the
// backend is naru-entry probe in the microVM, and the host is read back through naru-nftprobe: the
// addresses on its interfaces and its DNAT rules. The CCM, the Services and the Pods that terminate TLS
// (ADR 0085, criterion 10) land with the CCM add-on.
const (
	// The addresses are in the management prefix 10.78.0.0/24, so the client resolves them with ARP and
	// reaches the host that announced them.
	forwardAddr  = "10.78.0.80"
	proxyAddr    = "10.78.0.81"
	lanClient    = 1
	lanInterface = "eth0"
	plainNode    = 30080
	proxyNode    = 30443
)

var lanClientBinary = path.Join(fixture.GuestRoot, "bin/naru-lanclient")

// lanLine is the latest line the Node printed under key, without the key.
func lanLine(nd *node.Node, key string) string {
	lines := regexp.MustCompile(`(?m)naru94-`+key+`: (.*)$`).FindAllStringSubmatch(nd.Console(), -1)
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1][1]
}

// answerOf is what the client printed for target, or "" before it printed any.
func answerOf(nd *node.Node, target string) string {
	m := regexp.MustCompile(`(?:^|\s)` + regexp.QuoteMeta(target) + `=(\S+)`).FindStringSubmatch(lanLine(nd, "lan"))
	if m == nil {
		return ""
	}
	return m[1]
}

// withLANInterface gives naru-hostd the flag --lan-interface in the user-data: init writes no flag for it.
func withLANInterface(_ node.Config, file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	var visit func(n *yaml.Node)
	visit = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			var isHostd bool
			var args *yaml.Node
			for i := 0; i+1 < len(n.Content); i += 2 {
				switch n.Content[i].Value {
				case "name":
					isHostd = n.Content[i+1].Value == "naru-hostd"
				case "args":
					args = n.Content[i+1]
				}
			}
			if isHostd && args != nil && args.Kind == yaml.SequenceNode {
				args.Content = append(args.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "--lan-interface=" + lanInterface})
			}
		}
		for _, c := range n.Content {
			visit(c)
		}
	}
	visit(&doc)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o600)
}

func TestFleet_AClientOnTheLANReachesAMicroVMThroughALeasedAddressAndNotOnceTheLeaseEnds(t *testing.T) {
	targets := []string{forwardAddr + ":80", proxyAddr + ":443", forwardAddr + ":81"}
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2, vmMiB: 1024, capture: true, userData: withLANInterface,
		services: []node.ExtraService{
			{Name: "naru94-nftprobe", Binary: nftProbe, Env: map[string]string{"PATH": "/bin"}},
			{Name: "naru94-lanclient", Binary: lanClientBinary, Args: targets, Env: map[string]string{"PATH": "/bin"}},
		}})
	f.startOrch()
	f.enrollAndAdmit("eu-1", "disk=ssd")
	h1, h2 := 2, 3
	f.must(f.orch("network", "apply", "-f", f.networkFile(netA, prefixA)), "network apply "+netA)
	vm1, vm2 := f.netVMOn(h1, "a1", netA), f.netVMOn(h2, "a2", netA)

	ctx := context.Background()
	prov := func(i int) *hostapi.Client {
		c, err := hostcli.New(f.hostOfNode(i).Provisioner)
		require.NoError(t, err)
		return c
	}
	first, second := prov(h1), prov(h2)
	forward := func(vm netVM) hostapi.ExposureRequest {
		return hostapi.ExposureRequest{Network: netA, Backends: []string{vm.ip}, LeaseSeconds: 15,
			Ports: []hostapi.ExposurePort{{Port: 80, Protocol: "tcp", NodePort: plainNode}}}
	}
	proxied := func(vm netVM) hostapi.ExposureRequest {
		return hostapi.ExposureRequest{Network: netA, Backends: []string{vm.ip}, LeaseSeconds: 15, ProxyProtocol: "v2",
			Ports: []hostapi.ExposurePort{{Port: 443, Protocol: "tcp", NodePort: proxyNode}}}
	}
	client := f.node(lanClient)
	answer := func(target string) func() string { return func() string { return answerOf(client, target) } }
	reaches := func(target, prefix string) bool { return strings.HasPrefix(answerOf(client, target), prefix) }
	host1 := f.node(h1)

	// Both leases are taken on the first host.
	_, err := first.PutExposure(ctx, forwardAddr, forward(vm1))
	require.NoError(t, err)
	_, err = first.PutExposure(ctx, proxyAddr, proxied(vm1))
	require.NoError(t, err)

	// The client reaches the microVM through each, and the microVM reports the address it saw: for the
	// forwarded flow the source of the packet, for the proxied one the client's own address, which only
	// the PROXY header carries.
	eventually(t, "the client reaches the forwarded port", func() bool { return reaches(forwardAddr+":80", "src=10.") })
	eventually(t, "the client reaches the proxied port and the microVM names the client", func() bool {
		return reaches(proxyAddr+":443", "proxy-src="+nodeIP(lanClient)+":")
	})
	assert.Equal(t, "fail", answerOf(client, forwardAddr+":81"), "the control: a port the lease does not name is not forwarded")

	// The host holds what the lease says: the addresses on its interface and the DNAT of the forwarded
	// port to the microVM's NodePort, and none for the proxied one, which its own listener serves.
	eventually(t, "the host reports the addresses and the DNAT", func() bool {
		return strings.Contains(lanLine(host1, "lan-addr"), lanInterface+"=") &&
			strings.Contains(lanLine(host1, "lan-addr"), forwardAddr+"/32") && strings.Contains(lanLine(host1, "lan-addr"), proxyAddr+"/32") &&
			strings.Contains(lanLine(host1, "nft-dnat"), forwardAddr+":80/tcp=") &&
			strings.Contains(lanLine(host1, "nft-dnat"), vm1.ip+fmt.Sprintf(".%d", plainNode))
	})
	assert.NotContains(t, lanLine(host1, "nft-dnat"), proxyAddr, "the proxied exposure is not forwarded by the table")
	assert.NotContains(t, lanLine(f.node(h2), "lan-addr"), forwardAddr, "the second host does not hold it")

	// Renewed every 5 s, the leases outlive their 15 s.
	for range 4 {
		time.Sleep(5 * time.Second)
		_, err := first.PutExposure(ctx, forwardAddr, forward(vm1))
		require.NoError(t, err)
		_, err = first.PutExposure(ctx, proxyAddr, proxied(vm1))
		require.NoError(t, err)
	}
	assert.True(t, reaches(forwardAddr+":80", "src="), "the control: a renewed lease still answers after 20 s")

	// Deleted, the forwarded address leaves the interface and the table and the client no longer reaches
	// it, and the other lease is untouched.
	require.NoError(t, first.DeleteExposure(ctx, forwardAddr))
	eventually(t, "the address and its DNAT are gone from the first host", func() bool {
		return !strings.Contains(lanLine(host1, "lan-addr"), forwardAddr) && !strings.Contains(lanLine(host1, "nft-dnat"), forwardAddr)
	})
	eventually(t, "the client no longer reaches the forwarded port", func() bool { return answerOf(client, forwardAddr+":80") == "fail" })
	assert.True(t, reaches(proxyAddr+":443", "proxy-src="), "the control: the other lease still answers")

	// The address moves: the second host takes it, announces it, and the client reaches the microVM there.
	_, err = second.PutExposure(ctx, forwardAddr, forward(vm2))
	require.NoError(t, err)
	eventually(t, "the client reaches the forwarded port on the second host", func() bool {
		return reaches(forwardAddr+":80", "src=10.") && strings.Contains(lanLine(f.node(h2), "lan-addr"), forwardAddr+"/32")
	})
	assert.Contains(t, lanLine(f.node(h2), "nft-dnat"), vm2.ip+fmt.Sprintf(".%d", plainNode))

	// Not renewed, the leases end on both hosts, the addresses and the table with them.
	eventually(t, "the leases end and the client reaches nothing", func() bool {
		return answer(forwardAddr+":80")() == "fail" && answer(proxyAddr+":443")() == "fail"
	})
	for _, h := range []int{h1, h2} {
		nd := f.node(h)
		eventually(t, fmt.Sprintf("node-%d holds neither address nor rule", h), func() bool {
			return !strings.Contains(lanLine(nd, "lan-addr"), "10.78.0.8") && !strings.Contains(lanLine(nd, "nft-dnat"), "10.78.0.8")
		})
	}
	for _, c := range []*hostapi.Client{first, second} {
		got, err := c.Exposures(ctx)
		require.NoError(t, err)
		assert.Empty(t, got)
	}
}
