//go:build qemu_real

package tests_test

import (
	"bytes"
	"net/netip"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/pcap"
)

// The mesh scenario (#385, ADR 0113): orch gives each admitted host an address of the profile's mesh
// pool, hostd puts it on the tunnel, and a TCP connection from one host's mesh address to the
// other's crosses the tunnel. naru-meshprobe on every host answers on its mesh address and connects
// to the others'; the interface's counters and the capture show the connection went through
// WireGuard.
const (
	meshPool      = "10.99.0.0/29"
	meshPort      = "7900"
	meshInterface = "wg-naru"
)

// meshCandidates is every address the pool gives a host: the first is skipped.
var meshCandidates = []string{"10.99.0.1", "10.99.0.2", "10.99.0.3"}

var meshProbe = path.Join(fixture.GuestRoot, "bin/naru-meshprobe")

// meshReport is the latest line naru-meshprobe printed on the Node.
type meshReport struct {
	self    string
	answers map[string]string
	rx, tx  uint64
}

func meshOf(nd *node.Node) (meshReport, bool) {
	line := lanLine(nd, "mesh")
	if line == "" {
		return meshReport{}, false
	}
	r := meshReport{answers: map[string]string{}}
	for _, m := range regexp.MustCompile(`(\S+)=(\S+)`).FindAllStringSubmatch(line, -1) {
		switch m[1] {
		case "self":
			r.self = m[2]
		case "rx":
			r.rx, _ = strconv.ParseUint(m[2], 10, 64)
		case "tx":
			r.tx, _ = strconv.ParseUint(m[2], 10, 64)
		default:
			r.answers[m[1]] = m[2]
		}
	}
	return r, true
}

func TestFleet_AConnectionFromOneHostsMeshAddressReachesTheOthersThroughTheTunnel(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2, capture: true,
		services: []node.ExtraService{
			{Name: "naru94-meshprobe", Binary: meshProbe, Args: append([]string{meshInterface, meshPort}, meshCandidates...), Env: map[string]string{"PATH": "/bin"}},
		}})
	f.startOrch()

	// The profile carries the mesh pool before the hosts are admitted, so each takes its address at
	// admission.
	profile := filepath.Join(f.dir, "host-profile.yaml")
	f.must(nrun("gen", "host-profile", "--firecracker-sha256", inputs.FirecrackerSHA256,
		"--machine-pool", "10.77.0.0/16", "--mesh-pool", meshPool,
		"--orch-url", "https://"+nodeIP(1)+":"+orchPort, "--orch-ca-sha256", f.caSHA,
		"--host-management-prefix", managementPrefix, "--host-egress-interface", "eth0", "--host-tunnel-port", "51820",
		"--out", profile), "gen host-profile")
	f.must(f.orch("host", "profile", "set", "-f", profile), "host profile set")
	ids := f.enrollAndAdmit("eu-1")
	h1, h2 := 2, 3

	// Orch lists each host with its own address of the pool.
	pool := netip.MustParsePrefix(meshPool)
	given := map[string]string{}
	eventually(t, "orch lists both hosts with a mesh address", func() bool {
		for _, h := range f.hostList(1) {
			if h.MeshAddress != "" {
				given[h.ID] = h.MeshAddress
			}
		}
		return len(given) == 2
	})
	a, b := given[ids[h1]], given[ids[h2]]
	assert.NotEqual(t, a, b, "each host has its own")
	for _, addr := range []string{a, b} {
		assert.True(t, pool.Contains(netip.MustParseAddr(addr)), "%s is in the pool", addr)
	}

	// Each host's tunnel holds the address orch gave it, and a connection from it reaches the other's.
	var before map[int]meshReport
	eventually(t, "each host connects from its mesh address to the other's", func() bool {
		before = map[int]meshReport{}
		for i, pair := range map[int][2]string{h1: {a, b}, h2: {b, a}} {
			r, ok := meshOf(f.node(i))
			if !ok || r.self != pair[0] || r.answers[pair[1]] != pair[1] {
				return false
			}
			before[i] = r
		}
		return true
	})
	for _, i := range []int{h1, h2} {
		for _, addr := range meshCandidates {
			if addr != a && addr != b {
				assert.Equal(t, "fail", before[i].answers[addr], "the control: %s, which no host holds, answers %s nothing", addr, f.node(i).Name)
			}
		}
	}

	// The connections went through the tunnel: its counters grow with them, and what crosses between
	// the hosts is WireGuard, with the answers in none of its frames.
	time.Sleep(6 * time.Second)
	for _, i := range []int{h1, h2} {
		after, ok := meshOf(f.node(i))
		require.True(t, ok)
		assert.Greater(t, after.rx, before[i].rx, "%s: the tunnel received", f.node(i).Name)
		assert.Greater(t, after.tx, before[i].tx, "%s: the tunnel sent", f.node(i).Name)
	}
	capture, err := pcap.Read(f.node(1).CapturePath())
	require.NoError(t, err)
	between := capture.Between([]netip.Addr{netip.MustParseAddr(nodeIP(h1)), netip.MustParseAddr(nodeIP(h2))})
	require.NotEmpty(t, between, "the capture shows the two hosts' traffic")
	for _, p := range between {
		assert.True(t, p.IsWireGuard(51820), "%v -> %v proto %d ports %d -> %d is not WireGuard", p.Src, p.Dst, p.Proto, p.SrcPort, p.DstPort)
	}
	assert.False(t, bytes.Contains(capture.Raw, []byte("naru94-mesh-answer")), "the answer crossed in the clear")
}
