//go:build qemu_real

package tests_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/pcap"
)

// The network scenario (#100): one control Node and two hosts, networks neta and netb declared on orch
// with naructl, and a microVM of each network on each host. The microVMs run naru-entry's probe, which the
// test host drives through the harness route of the host the microVM is on: a microVM dials a TCP
// address, sends a datagram, sends a datagram with a forged source, and lists the datagrams it received.
// naru-nftprobe prints the host's nftables counters on its console, and the capture of the shared
// network shows what crosses between the hosts.

const (
	probePort  = "8080" // where naru-entry serves probe when a microVM runs it with no arguments
	prefixA    = "10.201.0.0/16"
	prefixB    = "10.202.0.0/16"
	netA, netB = "neta", "netb"
)

// netVM is a microVM of the scenario, and the loopback address of the test host that reaches its probe.
type netVM struct {
	name, ip string
	host     int // the number of the Node
	base     string
}

// probeBody is the request that creates a probing microVM on the network.
func probeBody(t *testing.T, name, network string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"name": name, "image": inputs.Image.Repository + "@" + inputs.Image.Digest,
		"cpu": 1, "memory": 256 << 20, "rootDiskBytes": 1 << 30, "network": network,
	})
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), name+".json")
	require.NoError(t, os.WriteFile(p, body, 0o600))
	return p
}

// hostOfNode is the host API of Node i.
func (f *fleet) hostOfNode(i int) *node.HostAPI {
	for _, h := range f.c.Hosts {
		if h.Index == i {
			return h
		}
	}
	f.t.Fatalf("node-%d runs no host API", i)
	return nil
}

// netVMOn creates the microVM on host Node i, which takes however long orch needs to declare the
// network on that host, and returns it with its address and the route to its probe.
func (f *fleet) netVMOn(i int, name, network string) netVM {
	t := f.t
	h := f.hostOfNode(i)
	file := probeBody(t, name, network)
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("the last answer of microvm create: " + last)
		}
	})
	eventually(t, name+" is created once orch has declared "+network+" on node-"+fmt.Sprint(i), func() bool {
		r := hctl(h.Provisioner, []string{"microvm", "create"}, "-f", file)
		last = fmt.Sprintf("%v: %s %s", r.Err, r.Stdout, r.Stderr)
		return r.Err == nil
	})
	var ip string
	eventually(t, name+" runs and has an address", func() bool {
		r := hctl(h.Reader, []string{"microvm", "get"}, name)
		var vm struct {
			State     string   `json:"state"`
			Addresses []string `json:"addresses"`
		}
		if r.Err != nil || json.Unmarshal([]byte(r.Stdout), &vm) != nil || vm.State != "running" || len(vm.Addresses) == 0 {
			return false
		}
		ip = strings.SplitN(vm.Addresses[0], "/", 2)[0]
		return true
	})
	addr, stop, err := h.RouteTo(context.Background(), ip+":"+probePort)
	require.NoError(t, err)
	t.Cleanup(stop)
	return netVM{name: name, ip: ip, host: i, base: "http://" + addr}
}

// try is the probe's answer to a request of one line, or false when it did not answer.
func (v netVM) try(pathQuery string) (string, bool) {
	resp, err := http.Get(v.base + pathQuery) //nolint:noctx // a test's request to a loopback listener
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b)), err == nil
}

// call is try, repeated until the probe answers.
func (v netVM) call(t *testing.T, pathQuery string) string {
	t.Helper()
	var body string
	eventually(t, v.name+" answers "+pathQuery, func() bool {
		var ok bool
		body, ok = v.try(pathQuery)
		return ok
	})
	return body
}

// dials is whether the microVM reaches the TCP address.
func (v netVM) dials(t *testing.T, target string) bool {
	t.Helper()
	return v.call(t, "/probe?target="+target) == "ok"
}

// tryDials is dials for a condition of eventually: it fails the condition, not the test.
func (v netVM) tryDials(target string) bool {
	body, ok := v.try("/probe?target=" + target)
	return ok && body == "ok"
}

// parseReceived reads the probe's /received: "<address> <count>" per line.
func parseReceived(body string) map[string]int {
	got := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		var addr string
		var n int
		if _, err := fmt.Sscanf(line, "%s %d", &addr, &n); err == nil {
			got[addr] = n
		}
	}
	return got
}

// received is the datagrams the microVM got, by source address.
func (v netVM) received(t *testing.T) map[string]int {
	t.Helper()
	return parseReceived(v.call(t, "/received"))
}

// counter is the packets of the nft counter whose comment is key (spaces as underscores), from the
// last line naru-nftprobe printed on the Node's console, or -1 when it printed none.
func counter(nd *node.Node, key string) int {
	lines := regexp.MustCompile(`(?m)^.*naru94-nft: (.*)$`).FindAllStringSubmatch(nd.Console(), -1)
	if len(lines) == 0 {
		return -1
	}
	m := regexp.MustCompile(`(?:^|\s)` + regexp.QuoteMeta(key) + `=(\d+)`).FindStringSubmatch(lines[len(lines)-1][1])
	if m == nil {
		return 0
	}
	var n int
	_, _ = fmt.Sscan(m[1], &n)
	return n
}

func (f *fleet) networkFile(name, prefix string) string {
	return f.file(name+".yaml", fmt.Sprintf("name: %s\nprefix: %s\n", name, prefix))
}

func TestFleet_NetworksAreSeparateMembershipSetsAndOnlyWireGuardCrossesBetweenTheHosts(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2, vmMiB: 1024, capture: true, services: []node.ExtraService{{Name: "naru94-nftprobe", Binary: nftProbe, Env: map[string]string{"PATH": "/bin"}}}})
	f.startOrch()
	f.enrollAndAdmit("eu-1", "disk=ssd")
	h1, h2 := 2, 3

	// Orch declares the networks; the leader tells every admitted host within --network-interval.
	for name, prefix := range map[string]string{netA: prefixA, netB: prefixB} {
		f.must(f.orch("network", "apply", "-f", f.networkFile(name, prefix)), "network apply "+name)
	}
	listed := f.must(f.orch("network", "list"), "network list")
	assert.Contains(t, listed, netA)
	assert.Contains(t, listed, netB)

	a1, a2 := f.netVMOn(h1, "a1", netA), f.netVMOn(h2, "a2", netA)
	b1, b2 := f.netVMOn(h1, "b1", netB), f.netVMOn(h2, "b2", netB)
	a1b := f.netVMOn(h1, "a1b", netA)
	for _, v := range []netVM{a1, a2, a1b} {
		assert.True(t, netip.MustParsePrefix(prefixA).Contains(netip.MustParseAddr(v.ip)), "%s is in net A's prefix", v.name)
	}
	for _, v := range []netVM{b1, b2} {
		assert.True(t, netip.MustParsePrefix(prefixB).Contains(netip.MustParseAddr(v.ip)), "%s is in net B's prefix", v.name)
	}

	// A to A answers on the same host, and across the hosts once the leader has declared the members
	// remote: the TCP dial and a datagram.
	eventually(t, "a1 reaches a2 across the hosts", func() bool { return a1.tryDials(a2.ip + ":" + probePort) })
	assert.True(t, a1.dials(t, a1b.ip+":"+probePort), "a1 reaches a1b on its own host")
	assert.Equal(t, "sent", a1.call(t, "/send?dst="+a2.ip))
	eventually(t, "a2 receives a1's datagram from a1's address", func() bool {
		body, _ := a2.try("/received")
		return parseReceived(body)[a1.ip] > 0
	})

	// These do not answer from A: B's microVMs on either host, both hosts' host API and Node API at the
	// host's address, the second NIC's and the gateway of the Machine prefix, and the management prefix.
	denied := []string{b1.ip + ":" + probePort, b2.ip + ":" + probePort, f.targetOf(1, "7500")}
	for _, h := range []int{h1, h2} {
		denied = append(denied,
			nodeIP(h)+":"+node.HostAPIPort, nodeIP(h)+":"+node.NodeAPIPort,
			node.HostNICAddress+":"+node.HostAPIPort,
			fmt.Sprintf("10.77.%d.1:%s", h, node.HostAPIPort), fmt.Sprintf("10.77.%d.1:%s", h, node.NodeAPIPort))
	}
	for _, target := range denied {
		assert.False(t, a1.dials(t, target), "a microVM of net A does not reach %s", target)
	}
	assert.False(t, a2.dials(t, b1.ip+":"+probePort), "nor a2 reaches b1")
	assert.True(t, b1.dials(t, b2.ip+":"+probePort), "the control: B reaches B across the hosts")
	hostNode := f.node(h1)
	eventually(t, "the host counts the drops", func() bool {
		return counter(hostNode, netA+"_to_another_network") > 0 && counter(hostNode, netA+"_to_the_host") > 0 && counter(hostNode, netA+"_to_management") > 0
	})

	// A forged source is dropped: an address outside every network and the address of a member of the
	// network on another host, which no local member holds. The forged datagram never arrives, and
	// the host's counter of forged sources rises.
	before := counter(hostNode, "forged_source")
	assert.Equal(t, "sent", a1.call(t, "/spoof?src=192.0.2.7&dst="+a2.ip))
	assert.Equal(t, "sent", a1.call(t, "/spoof?src="+a2.ip+"&dst="+a1b.ip))
	eventually(t, "the host counts the forged sources", func() bool { return counter(hostNode, "forged_source") >= before+2 })
	time.Sleep(3 * time.Second)
	assert.Zero(t, a2.received(t)["192.0.2.7"], "the outside source arrives nowhere")
	assert.Zero(t, a1b.received(t)[a2.ip], "the other member's address as a source arrives nowhere")
	assert.Greater(t, a2.received(t)[a1.ip], 0, "the control: a datagram from a1's own address arrives")

	// What crosses between the hosts is WireGuard: UDP to the tunnel port, and the payloads of the
	// datagrams and of the probe's answers are in none of the capture's frames.
	for range 5 {
		assert.Equal(t, "sent", a1.call(t, "/send?dst="+a2.ip))
		assert.True(t, a1.dials(t, a2.ip+":"+probePort))
	}
	time.Sleep(2 * time.Second)
	capture, err := pcap.Read(f.node(1).CapturePath())
	require.NoError(t, err)
	between := capture.Between([]netip.Addr{netip.MustParseAddr(nodeIP(h1)), netip.MustParseAddr(nodeIP(h2))})
	require.NotEmpty(t, between, "the capture shows the two hosts' traffic")
	for _, p := range between {
		assert.True(t, p.IsWireGuard(51820), "%v -> %v proto %d ports %d -> %d is not WireGuard", p.Src, p.Dst, p.Proto, p.SrcPort, p.DstPort)
	}
	for _, marker := range []string{"naru94-datagram", "naru94-forged", "naru91-entry-answer"} {
		assert.False(t, bytes.Contains(capture.Raw, []byte(marker)), "%q is in the capture, so it crossed in the clear", marker)
	}
}

// targetOf is address:port of control Node i.
func (f *fleet) targetOf(i int, port string) string { return nodeIP(i) + ":" + port }
