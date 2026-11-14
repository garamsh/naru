package network_test

import (
	"context"
	"errors"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/network"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		require.NoError(t, os.WriteFile(path, []byte(got), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(want), got)
}

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func addr(s string) netip.Addr  { return netip.MustParseAddr(s) }

func twoNetworks() network.Config {
	return network.Config{
		Networks: []network.Network{
			{Name: "b", Prefix: pfx("10.2.0.0/16")},
			{Name: "a", Prefix: pfx("10.1.0.0/16"), Endpoint: addr("10.1.0.2")},
		},
		Members: []network.Member{
			{Network: "a", Address: addr("10.1.0.3"), Routed: pfx("10.1.4.0/24")},
			{Network: "b", Address: addr("10.2.0.3")},
		},
		Peers: []network.Peer{
			{Name: "h2", PublicKey: "KEYTWO", Endpoint: netip.MustParseAddrPort("192.0.2.2:51820"), Prefixes: []netip.Prefix{pfx("10.2.0.0/24"), pfx("10.1.1.0/24")}},
			{Name: "h1", PublicKey: "KEYONE", Prefixes: []netip.Prefix{pfx("10.1.2.0/24")}},
		},
		Management: pfx("172.16.0.0/24"),
		HostAddrs:  []netip.Addr{addr("192.0.2.1"), addr("10.0.0.1")},
		TapPrefix:  "tap",
		Egress:     "eth0",
		Tunnel:     "wg-naru",
	}
}

func TestRuleset_TwoNetworks(t *testing.T) {
	golden(t, "two-networks.nft", network.Ruleset(twoNetworks()))
}

func TestRuleset_NoNetworkIsClosed(t *testing.T) {
	c := network.Config{TapPrefix: "tap"}
	golden(t, "empty.nft", network.Ruleset(c))
}

func TestRuleset_ClosedByDefaultAndCrossNetworkIsDropped(t *testing.T) {
	r := network.Ruleset(twoNetworks())
	assert.Contains(t, r, "policy drop")
	assert.Contains(t, r, "ip saddr 10.1.0.0/16 ip daddr { 10.2.0.0/16 } counter drop")
	assert.Contains(t, r, "ip saddr 10.2.0.0/16 ip daddr { 10.1.0.0/16 } counter drop")
	assert.Contains(t, r, "ip saddr 10.1.0.0/16 ip daddr 172.16.0.0/24 counter drop")
	assert.Contains(t, r, "ip saddr 10.1.0.0/16 ip daddr { 10.0.0.1, 192.0.2.1 } counter drop")
	assert.Contains(t, r, "forged source")
	assert.Contains(t, r, "ip saddr 172.16.0.0/24 ip daddr 10.1.0.2 accept")
	assert.NotContains(t, r, "ip saddr 172.16.0.0/24 ip daddr 10.2.0.", "management reaches only an API endpoint, and b has none")
}

func TestTunnelArgs(t *testing.T) {
	args := network.TunnelArgs(twoNetworks(), "/run/naru-hostd/wg.key", 51820, []string{"KEYTWO", "STALE"})
	golden(t, "tunnel.args", strings.Join(args, "\n")+"\n")
	assert.NotContains(t, strings.Join(args, " "), "PrivateKey", "the key is read from its file, not given")
}

type call struct {
	stdin string
	argv  []string
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	peers string
	fail  string
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{string(stdin), append([]string{name}, args...)})
	if f.fail == name {
		return nil, errors.New(name + " failed")
	}
	if name == "wg" && len(args) > 0 && args[0] == "show" {
		return []byte(f.peers), nil
	}
	return nil, nil
}

func newManager(f *fakeRunner) *network.Manager {
	c := twoNetworks()
	return &network.Manager{
		Run:     f,
		Base:    network.Config{Management: c.Management, HostAddrs: c.HostAddrs, TapPrefix: "tap", Egress: "eth0", Tunnel: "wg-naru"},
		KeyFile: "/run/naru-hostd/wg.key",
		Port:    51820,
	}
}

func TestManager_AppliesTheWholeStateOnEveryChange(t *testing.T) {
	f := &fakeRunner{peers: "STALE\n"}
	m := newManager(f)
	ctx := context.Background()
	require.NoError(t, m.Declare(ctx, network.Network{Name: "a", Prefix: pfx("10.1.0.0/16")}))
	require.NoError(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.1.0.3")}))
	require.NoError(t, m.SetPeers(ctx, []network.Peer{{PublicKey: "KEYONE", Prefixes: []netip.Prefix{pfx("10.1.2.0/24")}}}))

	last := f.calls[len(f.calls)-1]
	assert.Equal(t, []string{"wg", "set", "wg-naru", "listen-port", "51820", "private-key", "/run/naru-hostd/wg.key", "peer", "STALE", "remove", "peer", "KEYONE", "allowed-ips", "10.1.2.0/24"}, last.argv)
	var nft call
	for _, c := range f.calls {
		if c.argv[0] == "nft" {
			nft = c
		}
	}
	assert.Equal(t, []string{"nft", "-f", "-"}, nft.argv)
	assert.Contains(t, nft.stdin, "10.1.0.3", "the member is in the set of known sources")
	assert.Contains(t, nft.stdin, "ip saddr 10.1.0.0/16 ip daddr 10.1.0.0/16 accept")

	require.NoError(t, m.Detach(ctx, addr("10.1.0.3")))
	assert.NotContains(t, f.calls[len(f.calls)-3].stdin, "10.1.0.3", "a detached member is no longer a known source")
}

func TestManager_Refusals(t *testing.T) {
	f := &fakeRunner{}
	m := newManager(f)
	ctx := context.Background()
	require.NoError(t, m.Declare(ctx, network.Network{Name: "a", Prefix: pfx("10.1.0.0/16")}))

	assert.ErrorIs(t, m.Declare(ctx, network.Network{Name: "b", Prefix: pfx("10.1.5.0/24")}), network.ErrOverlap)
	assert.ErrorIs(t, m.Attach(ctx, network.Member{Network: "z", Address: addr("10.1.0.3")}), network.ErrUnknownNetwork)
	assert.ErrorIs(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.2.0.3")}), network.ErrOutside)
	assert.ErrorIs(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.1.0.3"), Routed: pfx("10.0.0.0/8")}), network.ErrOutside)
	_, ok := m.Network("b")
	assert.False(t, ok, "a refused network is not kept")
}

func TestManager_AFailedApplyChangesNothing(t *testing.T) {
	f := &fakeRunner{fail: "nft"}
	m := newManager(f)
	assert.Error(t, m.Declare(context.Background(), network.Network{Name: "a", Prefix: pfx("10.1.0.0/16")}))
	assert.Empty(t, m.Networks())

	f.fail = "wg"
	assert.Error(t, m.Declare(context.Background(), network.Network{Name: "a", Prefix: pfx("10.1.0.0/16")}))
	assert.Empty(t, m.Networks())
}

func TestManager_RemoveDropsTheNetworkAndItsMembers(t *testing.T) {
	f := &fakeRunner{}
	m := newManager(f)
	ctx := context.Background()
	require.NoError(t, m.Declare(ctx, network.Network{Name: "a", Prefix: pfx("10.1.0.0/16")}))
	require.NoError(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.1.0.3")}))
	require.NoError(t, m.Remove(ctx, "a"))
	assert.Empty(t, m.Networks())
	assert.NotContains(t, f.calls[len(f.calls)-3].stdin, "10.1.0.3")
	assert.NoError(t, m.Remove(ctx, "a"), "removing an absent network is not an error")
}
