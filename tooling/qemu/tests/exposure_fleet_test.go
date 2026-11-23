//go:build qemu_real

package tests_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/hostcli"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The exposure scenario (#275): a client on the LAN reaches a microVM of a network through a leased
// address, forwarded and through the PROXY protocol v2 listener of the host, and cannot once the lease
// is deleted, moved to the other host or not renewed. The client is naru-lanclient on every Node, the
// backend is naru-entry probe in the microVM, and the host is read back through naru-nftprobe: the
// addresses on its interfaces and its DNAT rules. The LoadBalancer scenario below drives the same path
// through orch, as the CCM does. The Services and the Pods that terminate TLS (ADR 0085, criterion 10) are
// not run here.
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

// The LoadBalancer scenario (#275, ADR 0085 item 10), driven as naru-ccm's service controller drives orch:
// an exposer of one network asks orch for an exposure, orch assigns the address from the operator's pool, a
// host announces it on the LAN, and a client outside the network reaches a microVM through it. The host that
// announces is stopped and the address moves to the other; the exposure is deleted and the address is freed.
// What it does not exercise is Kubernetes: the Services and the Nodes of a cluster. The cloud-provider's
// service controller, which turns a Service into the call below, is upstream code, and a cluster whose Nodes
// are microVMs placed by orch has no harness yet.
const (
	lbPool = "10.78.0.80/29"
	// lbAddress is the lowest address of lbPool, which orch assigns first.
	lbAddress = "10.78.0.80"
	lbService = "default.web.0a1b2c3d"
)

// exposerClient issues the credential of the network with naructl, as an operator does, and returns an orch
// client that presents it.
func (f *fleet) exposerClient(network string) *orchapi.Client {
	f.t.Helper()
	cert, key := filepath.Join(f.dir, "exposer.crt"), filepath.Join(f.dir, "exposer.key")
	f.must(nrun("ccm", "credential", "--network", network, "--admin", f.root.file, "--passphrase-file", f.pass,
		"--config", f.c.API.Admin, "--cert-out", cert, "--key-out", key), "ccm credential")
	pair, err := tls.LoadX509KeyPair(cert, key)
	require.NoError(f.t, err)
	pool := x509.NewCertPool()
	require.True(f.t, pool.AppendCertsFromPEM([]byte(f.orchCA)))
	cl, err := orchapi.New(f.orchURL(1), orchapi.HTTPClient(orchapi.TrustCA(pool, &pair), 30*time.Second))
	require.NoError(f.t, err)
	return cl
}

func orchStatus(err error) int {
	var e *orchapi.Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

func TestFleet_AnExposerGetsALANAddressFromOrchAndItMovesWithItsHostAndIsFreedOnDelete(t *testing.T) {
	target := lbAddress + ":80"
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2, vmMiB: 1024, capture: true, userData: withLANInterface,
		services: []node.ExtraService{
			{Name: "naru94-nftprobe", Binary: nftProbe, Env: map[string]string{"PATH": "/bin"}},
			{Name: "naru94-lanclient", Binary: lanClientBinary, Args: []string{target}, Env: map[string]string{"PATH": "/bin"}},
		}})
	f.startOrch()
	hostIDs := f.enrollAndAdmit("eu-1", "disk=ssd")
	h1, h2 := 2, 3
	f.must(f.orch("network", "apply", "-f", f.networkFile(netA, prefixA)), "network apply "+netA)
	vm1, vm2 := f.netVMOn(h1, "a1", netA), f.netVMOn(h2, "a2", netA)
	f.must(f.orch("address-pool", "apply", "-f", f.file("pool.yaml", "name: lan\ncidrs: ["+lbPool+"]\nnetworks: ["+netA+"]\n")), "address-pool apply")

	ctx := context.Background()
	exposer := f.exposerClient(netA)
	spec := orchapi.ExposureSpec{
		Network: netA, Pool: "lan", Backends: []string{vm1.ip, vm2.ip},
		Ports: []orchapi.ExposurePort{{Protocol: "TCP", Port: 80, NodePort: plainNode}},
	}
	client := f.node(lanClient)
	reaches := func() bool { return strings.HasPrefix(answerOf(client, target), "src=10.") }
	holds := func(i int) bool { return strings.Contains(lanLine(f.node(i), "lan-addr"), lbAddress+"/32") }
	announced := func(wantHost string) func() bool {
		return func() bool {
			e, err := exposer.Exposure(ctx, lbService)
			return err == nil && e.Phase == orchapi.ExposureAnnounced && (wantHost == "" || e.Host == wantHost)
		}
	}

	// The exposer asks; orch assigns the pool's lowest address and one host announces it.
	e, err := exposer.ApplyExposure(ctx, lbService, spec)
	require.NoError(t, err)
	require.Equal(t, lbAddress, e.Address, "the lowest free address of the pool")
	eventually(t, "a host announces the address", announced(""))
	holder := map[string]int{hostIDs[h1]: h1, hostIDs[h2]: h2}
	first, err := exposer.Exposure(ctx, lbService)
	require.NoError(t, err)
	holderNode := holder[first.Host]
	require.NotZero(t, holderNode, "the announcing host is one of the two")
	otherNode := h1 + h2 - holderNode
	eventually(t, "the client outside the network reaches a microVM through the address", reaches)
	assert.True(t, holds(holderNode), "the announcing host holds the address on its LAN interface")
	assert.False(t, holds(otherNode), "the other host does not: one host answers an address at a time")

	// The credential reaches its own network's exposures and nothing else of orch, whichever call is tried.
	_, err = exposer.Hosts(ctx)
	assert.Equal(t, 403, orchStatus(err), "an exposer reads no host")
	_, err = exposer.Groups(ctx)
	assert.Equal(t, 403, orchStatus(err), "nor a group")
	_, err = exposer.ApplyExposure(ctx, "other", orchapi.ExposureSpec{Network: netB, Pool: "lan", Backends: []string{vm1.ip},
		Ports: []orchapi.ExposurePort{{Protocol: "TCP", Port: 80, NodePort: plainNode}}})
	assert.Equal(t, 403, orchStatus(err), "nor another network's exposure")
	_, err = exposer.ApplyExposure(ctx, "wild", orchapi.ExposureSpec{Network: netA, Pool: "lan", Backends: []string{"10.250.0.1"},
		Ports: []orchapi.ExposurePort{{Protocol: "TCP", Port: 80, NodePort: plainNode}}})
	assert.Equal(t, 422, orchStatus(err), "a backend that is no member of the network is refused")
	_, err = exposer.NetworkMembers(ctx, netB)
	assert.Equal(t, 404, orchStatus(err), "another network's members are not there")

	// The announcing host is lost: the address moves to the other host after the lease and the skew, and the
	// client reaches the microVM that runs there.
	f.node(holderNode).Stop()
	eventually(t, "the address is announced by the other host", announced(hostIDs[otherNode]))
	eventually(t, "the other host holds it and the client reaches a microVM again", func() bool { return holds(otherNode) && reaches() })
	assert.Contains(t, lanLine(f.node(otherNode), "nft-dnat"), []string{vm1.ip, vm2.ip}[otherNode-h1],
		"it forwards to the microVM that runs on it")

	// Deleted, the address leaves the host and the client no longer reaches it, and orch frees it: another
	// exposure is given the same address.
	require.NoError(t, exposer.DeleteExposure(ctx, lbService))
	eventually(t, "the host removed the address", func() bool { return !holds(otherNode) })
	eventually(t, "the client reaches nothing", func() bool { return answerOf(client, target) == "fail" })
	// An exposer no longer sees the record at once; a reader's list shows it while it releases, so the wait is on that list.
	eventually(t, "orch freed the address: the reader's list no longer holds the exposure", func() bool {
		list := f.orch("exposure", "list")
		return list.Err == nil && !strings.Contains(list.Stdout, lbService)
	})
	again, err := exposer.ApplyExposure(ctx, "again", spec)
	require.NoError(t, err)
	assert.Equal(t, lbAddress, again.Address, "the control: the freed address is the lowest free one again")
}

func TestFleet_ARevokedExposersCredentialIsRefusedAtOnce(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1, vmMiB: 512})
	f.startOrch()
	f.enrollAndAdmit("eu-1", "disk=ssd")
	f.must(f.orch("network", "apply", "-f", f.networkFile(netA, prefixA)), "network apply "+netA)
	ctx := context.Background()
	exposer := f.exposerClient(netA)
	_, err := exposer.Exposures(ctx)
	require.NoError(t, err, "the control: the credential is accepted before it is revoked")

	serial := serialOfFile(t, filepath.Join(f.dir, "exposer.crt"))
	f.must(nrun("operator", "revoke", "--admin", f.root.file, "--passphrase-file", f.pass, "--config", f.c.API.Admin, "--serial", serial), "revoke the exposer")
	// The refusal is the handshake's: orch's verifier rejects the revoked chain (ADR 0062) and the client hears
	// the TLS alert, not an HTTP answer and not a timeout.
	var last error
	eventually(t, "orch refuses the revoked pair at the handshake", func() bool {
		_, last = exposer.Exposures(ctx)
		return last != nil && strings.Contains(last.Error(), "bad certificate")
	})
	assert.Zero(t, orchStatus(last), "an alert, not an HTTP status")
	assert.NotErrorIs(t, last, context.DeadlineExceeded, "not a timeout")
}
