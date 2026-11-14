package api

import (
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/hostapi"
)

// fakeTunnel is the network.Tunnel of the tests: it records the peers it was set to.
type fakeTunnel struct {
	peers []network.Peer
	fail  bool
}

func (f *fakeTunnel) PublicKey() string { return "PUBLICKEYOFTHISHOST" }
func (f *fakeTunnel) Port() int         { return 51820 }
func (f *fakeTunnel) Set(p []network.Peer) error {
	if f.fail {
		return errors.New("the tunnel is down")
	}
	f.peers = p
	return nil
}

func TestTunnel_ReaderReportsTheKeyPortAndMachinePrefixes(t *testing.T) {
	e := start(t)
	r := e.as(t, ReaderOrganization)
	code, b := e.do(t, r, "GET", "/v1/tunnel", nil)
	require.Equal(t, 200, code, string(b))
	var got hostapi.Tunnel
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, hostapi.Tunnel{PublicKey: "PUBLICKEYOFTHISHOST", ListenPort: 51820, MachinePrefixes: []string{"10.1.0.0/24"}}, got)
	assert.NotContains(t, string(b), "rivate", "the body carries no private key")

	code, _ = e.do(t, e.as(t, "other"), "GET", "/v1/tunnel", nil)
	assert.Equal(t, 403, code, "a certificate with no role of this API reads nothing")

	e.nets.Tunnel = nil
	code, _ = e.do(t, r, "GET", "/v1/tunnel", nil)
	assert.Equal(t, 501, code, "a host started without a tunnel has none to report")

	none := startEnv(t, netip.MustParseAddr("10.1.0.1"), false)
	code, _ = none.do(t, none.as(t, ReaderOrganization), "GET", "/v1/tunnel", nil)
	assert.Equal(t, 501, code, "a host without networks has no tunnel")
}

func TestNetwork_RemoteMembers(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	code, b := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.8.0.0/16", RemoteMembers: []string{"10.0.2.7/32", "10.8.2.0/24"}})
	require.Equal(t, 200, code, string(b))
	var got hostapi.Network
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, []string{"10.0.2.7/32", "10.8.2.0/24"}, got.RemoteMembers)
	assert.Contains(t, e.nft.ruleset(), "elements = { 10.0.2.7, 10.8.0.0/16 }", "the remote member is in the network's set; the pod prefix inside the prefix is not written twice")

	for name, bad := range map[string][]string{
		"not a prefix": {"ten"},
		"not masked":   {"10.0.2.7/24"},
		"ipv6":         {"fd00::/64"},
	} {
		code, b := e.do(t, p, "PUT", "/v1/networks/z", hostapi.NetworkRequest{Prefix: "10.5.0.0/16", RemoteMembers: bad})
		assert.Equal(t, 422, code, "%s: %s", name, b)
		assert.Contains(t, string(b), "remoteMembers[0]", name)
	}
	_, ok := e.nets.Network("z")
	assert.False(t, ok, "a refused network is not kept")

	code, _ = e.do(t, p, "PUT", "/v1/networks/z", hostapi.NetworkRequest{Prefix: "10.5.0.0/16", RemoteMembers: []string{"10.0.3.7/32"}})
	assert.Equal(t, 200, code, "control: a valid member is accepted")
}

func TestMicroVM_ReportsItsNetworkAndPodPrefix(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	r := e.as(t, ReaderOrganization)
	code, _ := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	require.Equal(t, 200, code)

	body := osBody()
	body["network"] = "a"
	body["podPrefix"] = "10.1.7.0/24"
	code, b := e.do(t, p, "POST", "/v1/microvms", body)
	require.Equal(t, 201, code, string(b))

	code, b = e.do(t, r, "GET", "/v1/microvms/web", nil)
	require.Equal(t, 200, code, string(b))
	got := microVMOf(t, b)
	assert.Equal(t, "a", got.Network)
	assert.Equal(t, "10.1.7.0/24", got.PodPrefix)

	code, b = e.do(t, r, "GET", "/v1/microvms", nil)
	require.Equal(t, 200, code, string(b))
	var list struct{ MicroVMs []hostapi.MicroVM }
	require.NoError(t, json.Unmarshal(b, &list))
	require.Len(t, list.MicroVMs, 1)
	assert.Equal(t, "a", list.MicroVMs[0].Network)
	assert.Equal(t, "10.1.7.0/24", list.MicroVMs[0].PodPrefix)

	// A microVM that hostd did not attach reports no network.
	e.rt.machines["bare"] = mvmd.Machine{ID: "bare", State: mvmd.StateRunning, Addresses: []string{"10.1.0.9"}}
	code, b = e.do(t, r, "GET", "/v1/microvms/bare", nil)
	require.Equal(t, 200, code, string(b))
	assert.Empty(t, microVMOf(t, b).Network, "control: a microVM on no network names none")
}

func TestResume_AttachesToANetworkAndRollsBackWhenItCannot(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	require.Equal(t, 200, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "s"})
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	require.Equal(t, 204, code)

	// Refused before mvmd is called: an unknown network, a bad name, a pod prefix outside the prefix.
	for name, req := range map[string]hostapi.ResumeRequest{
		"unknown network": {Name: "w2", Network: "nowhere"},
		"bad network":     {Name: "w2", Network: "Bad_"},
		"pod outside":     {Name: "w2", Network: "a", PodPrefix: "10.7.0.0/24"},
	} {
		code, b := e.do(t, c, "POST", "/v1/snapshots/s/resume", req)
		assert.Equal(t, 422, code, "%s: %s", name, b)
	}
	assert.NotContains(t, e.rt.calls, "restore w2")

	// The tunnel fails, so the member cannot be added: the restored microVM is removed, its disks stay.
	e.tunnel.fail = true
	code, b := e.do(t, c, "POST", "/v1/snapshots/s/resume", hostapi.ResumeRequest{Name: "w2", Network: "a"})
	assert.Equal(t, 502, code, string(b))
	assert.NotContains(t, e.rt.machines, "w2", "the rolled-back microVM is gone")
	assert.Contains(t, e.rt.disks, "web.data", "the snapshot's disk is left for the next attempt")
	assert.Empty(t, e.nets.Members())

	e.tunnel.fail = false
	code, b = e.do(t, c, "POST", "/v1/snapshots/s/resume", hostapi.ResumeRequest{Name: "w2", Network: "a", PodPrefix: "10.1.8.0/24"})
	require.Equal(t, 201, code, string(b))
	got := microVMOf(t, b)
	assert.Equal(t, "a", got.Network)
	assert.Equal(t, "10.1.8.0/24", got.PodPrefix)
	assert.Equal(t, []network.Member{{Network: "a", Address: netip.MustParseAddr("10.1.0.6"), Routed: netip.MustParsePrefix("10.1.8.0/24")}}, e.nets.Members())
}

func TestMicroVMDelete_KeepsTheDisksASnapshotNeeds(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "s"})
	require.Equal(t, 201, code)

	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	require.Equal(t, 204, code)
	assert.NotContains(t, e.rt.machines, "web")
	assert.Contains(t, e.rt.disks, "web.data", "the snapshot records this disk")

	// The snapshot is still resumable after its source is gone.
	code, b := e.do(t, c, "POST", "/v1/snapshots/s/resume", hostapi.ResumeRequest{Name: "w2"})
	assert.Equal(t, 201, code, string(b))

	// A delete of an absent microVM does not remove the disks of its snapshot either.
	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	assert.Equal(t, 404, code)
	assert.Contains(t, e.rt.disks, "web.data")
}

func TestMicroVMDelete_RemovesItsDisksWhenNoSnapshotNeedsThem(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	require.Equal(t, 204, code)
	assert.NotContains(t, e.rt.disks, "web.data", "control: with no snapshot the disk goes with the microVM")
}

func TestSnapshotDelete_RemovesTheDisksNobodyElseNeeds(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	for _, n := range []string{"s1", "s2"} {
		code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: n})
		require.Equal(t, 201, code)
	}
	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	require.Equal(t, 204, code)

	code, _ = e.do(t, c, "DELETE", "/v1/snapshots/s1", nil)
	require.Equal(t, 204, code)
	assert.Contains(t, e.rt.disks, "web.data", "the other snapshot still records it")

	code, _ = e.do(t, c, "DELETE", "/v1/snapshots/s2", nil)
	require.Equal(t, 204, code)
	assert.NotContains(t, e.rt.disks, "web.data", "the last snapshot that records a disk takes it away")
}

func TestSnapshotDelete_KeepsTheDisksOfALiveMicroVM(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "s"})
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "DELETE", "/v1/snapshots/s", nil)
	require.Equal(t, 204, code)
	assert.Contains(t, e.rt.disks, "web.data", "web still holds it")
}

func TestNetwork_StateSurvivesARestartOfTheHost(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16", RemoteMembers: []string{"10.0.2.7/32"}})
	require.Equal(t, 200, code)
	body := createBody()
	body["network"] = "a"
	code, _ = e.do(t, p, "POST", "/v1/microvms", body)
	require.Equal(t, 201, code)
	code, _ = e.do(t, p, "PUT", "/v1/peers", hostapi.PeersRequest{Peers: []hostapi.Peer{{Name: "h2", PublicKey: keyOne, Prefixes: []string{"10.2.0.0/24"}}}})
	require.Equal(t, 200, code)
	before := e.nft.ruleset()

	// The daemon starts again: a Manager of the same state file holds nothing until Load applies it.
	e.nft = &recorder{}
	e.tunnel = &fakeTunnel{}
	again := e.manager()
	require.NoError(t, again.Load(t.Context()))
	assert.Equal(t, e.nets.Networks(), again.Networks())
	assert.Equal(t, e.nets.Members(), again.Members())
	assert.Equal(t, before, e.nft.ruleset(), "the ruleset applied at start is the one applied before")
	assert.Len(t, e.tunnel.peers, 1)
}

func TestNetwork_ARefusedMemberLeavesNothingBehindInTheInputChain(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	require.Equal(t, 200, code)
	rules := e.nft.ruleset()
	assert.Contains(t, rules, `ct state established,related accept`)
	assert.Contains(t, rules, `iifname "tap*" counter drop comment "microVM to the host"`, "a microVM reaches no address of the host")
	assert.Contains(t, rules, `ip saddr @net_a counter drop comment "a to the host"`)
}
