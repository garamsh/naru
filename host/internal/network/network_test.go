package network_test

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

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

// twoNetworks is two networks on one host whose microVMs' addresses are both in the Machine prefix
// 10.0.0.0/24, which mvmd assigns from, and whose prefixes are apart.
func twoNetworks() network.Config {
	return network.Config{
		Networks: []network.Network{
			{Name: "b", Prefix: pfx("10.9.0.0/16")},
			{Name: "a", Prefix: pfx("10.8.0.0/16"), Endpoint: addr("10.8.0.2"), Remote: []netip.Prefix{pfx("10.0.2.7/32"), pfx("10.8.2.0/24"), pfx("10.8.2.5/32")}},
		},
		Members: []network.Member{
			{Network: "a", Address: addr("10.0.0.3"), Routed: pfx("10.8.1.0/24")},
			{Network: "b", Address: addr("10.0.0.4"), Routed: pfx("10.9.1.0/24")},
		},
		Management: pfx("172.16.0.0/24"),
		TapPrefix:  "tap",
		Egress:     "eth0",
	}
}

func TestRuleset_TwoNetworks(t *testing.T) {
	golden(t, "two-networks.nft", network.Ruleset(twoNetworks()))
}

func TestRuleset_NoNetworkIsClosed(t *testing.T) {
	golden(t, "empty.nft", network.Ruleset(network.Config{TapPrefix: "tap"}))
}

func TestRuleset_SetsHoldNoOverlap(t *testing.T) {
	// nft refuses a set with overlapping intervals; a remote member inside the prefix, or a member
	// named twice, is written once.
	c := twoNetworks()
	c.Networks[1].Remote = append(c.Networks[1].Remote, pfx("10.8.9.9/32"), pfx("10.8.2.0/24"))
	tb := parseTable(t, network.Ruleset(c))
	assert.Equal(t, []netip.Prefix{pfx("10.0.0.3/32"), pfx("10.0.2.7/32"), pfx("10.8.0.0/16")}, tb.sets["net_a"])
}

func TestRuleset_NetworksAreSetsNotPrefixes(t *testing.T) {
	tb := parseTable(t, network.Ruleset(twoNetworks()))
	const gateway = "10.0.0.1"
	for _, tc := range []struct {
		name   string
		p      packet
		accept bool
		rule   string
	}{
		// Both microVMs' addresses are in 10.0.0.0/24, which no prefix of a network holds: only the sets tell them apart.
		{"a to b by address", packet{in: "tap1", out: "tap2", src: "10.0.0.3", dst: "10.0.0.4"}, false, "a to another network"},
		{"b to a by address", packet{in: "tap2", out: "tap1", src: "10.0.0.4", dst: "10.0.0.3"}, false, "b to another network"},
		{"a pod to b pod", packet{in: "tap1", out: "tap2", src: "10.8.1.7", dst: "10.9.1.7"}, false, "a to another network"},
		{"a address to its own pod prefix", packet{in: "tap1", out: "tap1", src: "10.0.0.3", dst: "10.8.1.7"}, true, "a inside its set"},
		{"a pod to its own address", packet{in: "tap1", out: "tap1", src: "10.8.1.7", dst: "10.0.0.3"}, true, "a inside its set"},
		{"a to the egress", packet{in: "tap1", out: "eth0", src: "10.0.0.3", dst: "198.51.100.1"}, true, "a egress"},
		{"a to management", packet{in: "tap1", out: "eth1", src: "10.0.0.3", dst: "172.16.0.9"}, false, "a to management"},
		{"an address of no member", packet{in: "tap1", out: "tap2", src: "10.0.0.3", dst: "10.0.0.9"}, false, "policy drop"},
		{"a reply", packet{in: "tap2", out: "tap1", src: "10.0.0.4", dst: "10.0.0.3", established: true}, true, ""},
		{"management to a's endpoint", packet{in: "eth1", out: "tap1", src: "172.16.0.9", dst: "10.8.0.2"}, true, ""},
		{"management to b", packet{in: "eth1", out: "tap2", src: "172.16.0.9", dst: "10.0.0.4"}, false, "policy drop"},
	} {
		got := tb.judge("forward", tc.p)
		assert.Equal(t, tc.accept, got.accept, tc.name)
		if tc.rule != "" {
			assert.Equal(t, tc.rule, got.rule, tc.name)
		}
	}
	_ = gateway
}

func TestRuleset_RemoteMembers(t *testing.T) {
	tb := parseTable(t, network.Ruleset(twoNetworks()))
	for _, tc := range []struct {
		name   string
		p      packet
		accept bool
		rule   string
	}{
		{"to a remote address of a", packet{in: "tap1", out: "wg-naru", src: "10.0.0.3", dst: "10.0.2.7"}, true, "a inside its set"},
		{"to a remote pod prefix of a", packet{in: "tap1", out: "wg-naru", src: "10.8.1.7", dst: "10.8.2.77"}, true, "a inside its set"},
		{"from a remote address of a", packet{in: "wg-naru", out: "tap1", src: "10.0.2.7", dst: "10.0.0.3"}, true, "a inside its set"},
		{"from a remote pod prefix of a", packet{in: "wg-naru", out: "tap1", src: "10.8.2.77", dst: "10.8.1.7"}, true, "a inside its set"},
		{"b to a remote member of a", packet{in: "tap2", out: "wg-naru", src: "10.0.0.4", dst: "10.0.2.7"}, false, "b to another network"},
		{"a remote member of a to b", packet{in: "wg-naru", out: "tap2", src: "10.0.2.7", dst: "10.0.0.4"}, false, "a to another network"},
		{"a to an address that is nobody's", packet{in: "tap1", out: "wg-naru", src: "10.0.0.3", dst: "10.0.2.99"}, false, "policy drop"},
	} {
		got := tb.judge("forward", tc.p)
		assert.Equal(t, tc.accept, got.accept, tc.name)
		assert.Equal(t, tc.rule, got.rule, tc.name)
	}
}

func TestRuleset_ForgedSource(t *testing.T) {
	tb := parseTable(t, network.Ruleset(twoNetworks()))
	for _, tc := range []struct {
		name   string
		src    string
		accept bool
	}{
		{"a's own address", "10.0.0.3", true},
		{"a's own pod prefix", "10.8.1.200", true},
		{"an address no member holds", "10.0.0.77", false},
		{"a free address in the network's prefix", "10.8.200.1", false},
		{"a remote member's address", "10.0.2.7", false},
		{"a remote member's pod prefix", "10.8.2.5", false},
		{"the management plane", "172.16.0.9", false},
	} {
		got := tb.judge("forward", packet{in: "tap1", out: "tap1", src: tc.src, dst: "10.0.0.3"})
		assert.Equal(t, tc.accept, got.accept, tc.name)
		if !tc.accept {
			assert.Equal(t, "forged source", got.rule, tc.name)
		}
	}
	// The same sources arriving by the tunnel are not from a tap: the peer's allowed IPs bound them.
	assert.True(t, tb.judge("forward", packet{in: "wg-naru", out: "tap1", src: "10.0.2.7", dst: "10.0.0.3"}).accept)
}

func TestRuleset_NoMemberAcceptsNoTapSource(t *testing.T) {
	c := twoNetworks()
	c.Members = nil
	tb := parseTable(t, network.Ruleset(c))
	got := tb.judge("forward", packet{in: "tap1", out: "tap1", src: "10.8.1.7", dst: "10.8.1.8"})
	assert.False(t, got.accept)
	assert.Equal(t, "forged source", got.rule)
}

func TestRuleset_InputChain(t *testing.T) {
	tb := parseTable(t, network.Ruleset(twoNetworks()))
	for _, tc := range []struct {
		name   string
		p      packet
		accept bool
		rule   string
	}{
		{"a microVM to the gateway address", packet{in: "tap1", src: "10.0.0.3", dst: "10.0.0.1"}, false, "microVM to the host"},
		{"a microVM to the host's management address", packet{in: "tap1", src: "10.0.0.3", dst: "172.16.0.1"}, false, "microVM to the host"},
		{"a microVM of b to the gateway address", packet{in: "tap2", src: "10.0.0.4", dst: "10.0.0.1"}, false, "microVM to the host"},
		{"a reply of the host to a microVM's connection", packet{in: "tap1", src: "10.0.0.3", dst: "10.0.0.1", established: true}, true, ""},
		{"a remote member by the tunnel to the host", packet{in: "wg-naru", src: "10.0.2.7", dst: "10.0.0.1"}, false, "a to the host"},
		{"the management plane to the host", packet{in: "eth1", src: "172.16.0.9", dst: "172.16.0.1"}, true, "policy accept"},
		{"a peer's handshake", packet{in: "eth0", src: "192.0.2.2", dst: "192.0.2.1"}, true, "policy accept"},
	} {
		got := tb.judge("input", tc.p)
		assert.Equal(t, tc.accept, got.accept, tc.name)
		if tc.rule != "" {
			assert.Equal(t, tc.rule, got.rule, tc.name)
		}
	}
}

type call struct {
	stdin string
	argv  []string
}

type fakeRunner struct {
	mu    sync.Mutex
	calls []call
	fail  bool
}

func (f *fakeRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{string(stdin), append([]string{name}, args...)})
	if f.fail {
		return nil, errors.New(name + " failed")
	}
	return nil, nil
}

func (f *fakeRunner) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return ""
	}
	return f.calls[len(f.calls)-1].stdin
}

// fakeTunnel is the Tunnel of the tests: it records the peers it was set to.
type fakeTunnel struct {
	peers []network.Peer
	fail  bool
}

func (f *fakeTunnel) PublicKey() string { return "PUBLIC" }
func (f *fakeTunnel) Port() int         { return 51820 }
func (f *fakeTunnel) Set(p []network.Peer) error {
	if f.fail {
		return errors.New("tunnel failed")
	}
	f.peers = p
	return nil
}

func newManager(f *fakeRunner, tun *fakeTunnel, state string) *network.Manager {
	c := twoNetworks()
	return &network.Manager{
		Run: f, Tunnel: tun, State: state,
		Base: network.Config{Management: c.Management, TapPrefix: "tap", Egress: "eth0"},
	}
}

func TestManager_AppliesTheWholeStateOnEveryChange(t *testing.T) {
	f, tun := &fakeRunner{}, &fakeTunnel{}
	m := newManager(f, tun, "")
	ctx := context.Background()
	require.NoError(t, m.Declare(ctx, network.Network{Name: "a", Prefix: pfx("10.8.0.0/16"), Remote: []netip.Prefix{pfx("10.0.2.7/32")}}))
	require.NoError(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.0.0.3"), Routed: pfx("10.8.1.0/24")}))
	peers := []network.Peer{{PublicKey: "KEYONE", Prefixes: []netip.Prefix{pfx("10.8.2.0/24")}}}
	require.NoError(t, m.SetPeers(ctx, peers))

	assert.Equal(t, []string{"nft", "-f", "-"}, f.calls[len(f.calls)-1].argv)
	tb := parseTable(t, f.last())
	assert.Equal(t, []netip.Prefix{pfx("10.0.0.3/32"), pfx("10.0.2.7/32"), pfx("10.8.0.0/16")}, tb.sets["net_a"], "the member and the remote member are in the set")
	assert.Equal(t, peers, tun.peers, "the tunnel was given the whole peer list")
	for _, c := range f.calls {
		assert.Equal(t, "nft", c.argv[0], "the host runs nothing but nft")
	}

	require.NoError(t, m.Detach(ctx, addr("10.0.0.3")))
	assert.NotContains(t, parseTable(t, f.last()).sets["net_a"], pfx("10.0.0.3/32"), "a detached member is no longer in the set")
}

func TestManager_Refusals(t *testing.T) {
	m := newManager(&fakeRunner{}, &fakeTunnel{}, "")
	ctx := context.Background()
	require.NoError(t, m.Declare(ctx, network.Network{Name: "a", Prefix: pfx("10.8.0.0/16")}))

	assert.ErrorIs(t, m.Declare(ctx, network.Network{Name: "b", Prefix: pfx("10.8.5.0/24")}), network.ErrOverlap)
	assert.ErrorIs(t, m.Attach(ctx, network.Member{Network: "z", Address: addr("10.0.0.3")}), network.ErrUnknownNetwork)
	assert.ErrorIs(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.0.0.3"), Routed: pfx("10.0.0.0/24")}), network.ErrOutside)
	assert.ErrorIs(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.0.0.3"), Routed: pfx("10.0.0.0/8")}), network.ErrOutside)
	_, ok := m.Network("b")
	assert.False(t, ok, "a refused network is not kept")

	// An address outside the prefix is the normal case: mvmd chose it from the Machine prefix.
	assert.NoError(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.0.0.3"), Routed: pfx("10.8.1.0/24")}))

	// A prefix that would leave a member's pod prefix outside is refused, and the network stays as it was.
	assert.ErrorIs(t, m.Declare(ctx, network.Network{Name: "a", Prefix: pfx("10.8.8.0/24")}), network.ErrOutside)
	n, _ := m.Network("a")
	assert.Equal(t, pfx("10.8.0.0/16"), n.Prefix)
}

func TestManager_AFailedApplyChangesNothing(t *testing.T) {
	f, tun := &fakeRunner{fail: true}, &fakeTunnel{}
	m := newManager(f, tun, "")
	assert.Error(t, m.Declare(context.Background(), network.Network{Name: "a", Prefix: pfx("10.8.0.0/16")}))
	assert.Empty(t, m.Networks())

	f.fail, tun.fail = false, true
	assert.Error(t, m.Declare(context.Background(), network.Network{Name: "a", Prefix: pfx("10.8.0.0/16")}))
	assert.Empty(t, m.Networks())
}

func TestManager_RemoveDropsTheNetworkAndItsMembers(t *testing.T) {
	f := &fakeRunner{}
	m := newManager(f, &fakeTunnel{}, "")
	ctx := context.Background()
	require.NoError(t, m.Declare(ctx, network.Network{Name: "a", Prefix: pfx("10.8.0.0/16")}))
	require.NoError(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.0.0.3")}))
	require.NoError(t, m.Remove(ctx, "a"))
	assert.Empty(t, m.Networks())
	assert.Empty(t, m.Members())
	assert.NotContains(t, f.last(), "10.0.0.3")
	assert.NoError(t, m.Remove(ctx, "a"), "removing an absent network is not an error")
}

func TestManager_TunnelInfo(t *testing.T) {
	m := newManager(&fakeRunner{}, &fakeTunnel{}, "")
	got, ok := m.TunnelInfo()
	assert.True(t, ok)
	assert.Equal(t, network.TunnelInfo{PublicKey: "PUBLIC", Port: 51820}, got)

	m.Tunnel = nil
	_, ok = m.TunnelInfo()
	assert.False(t, ok, "a host with no tunnel has no key to report")
}

func TestManager_StateSurvivesARestart(t *testing.T) {
	state := filepath.Join(t.TempDir(), "hostd", "networks.json")
	ctx := context.Background()
	f1, tun1 := &fakeRunner{}, &fakeTunnel{}
	m := newManager(f1, tun1, state)
	require.NoError(t, m.Declare(ctx, network.Network{Name: "a", Prefix: pfx("10.8.0.0/16"), Endpoint: addr("10.8.0.2"), Remote: []netip.Prefix{pfx("10.0.2.7/32")}}))
	require.NoError(t, m.Declare(ctx, network.Network{Name: "b", Prefix: pfx("10.9.0.0/16")}))
	require.NoError(t, m.Attach(ctx, network.Member{Network: "a", Address: addr("10.0.0.3"), Routed: pfx("10.8.1.0/24")}))
	require.NoError(t, m.Attach(ctx, network.Member{Network: "b", Address: addr("10.0.0.4")}))
	peers := []network.Peer{{Name: "h2", PublicKey: "KEYTWO", Endpoint: netip.MustParseAddrPort("192.0.2.2:51820"), Prefixes: []netip.Prefix{pfx("10.0.2.0/24"), pfx("10.8.2.0/24")}}}
	require.NoError(t, m.SetPeers(ctx, peers))
	require.NoError(t, m.Detach(ctx, addr("10.0.0.4")))

	st, err := os.Stat(state)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	left, err := os.ReadDir(filepath.Dir(state))
	require.NoError(t, err)
	assert.Len(t, left, 1, "no temporary file is left beside the state")

	// A new Manager, as after the daemon restarts: it holds nothing until Load, and then the same state, applied.
	f2, tun2 := &fakeRunner{}, &fakeTunnel{}
	again := newManager(f2, tun2, state)
	assert.Empty(t, again.Networks())
	require.NoError(t, again.Load(ctx))
	assert.Equal(t, m.Networks(), again.Networks())
	assert.Equal(t, m.Members(), again.Members())
	assert.Equal(t, m.Peers(), again.Peers())
	assert.Equal(t, f1.last(), f2.last(), "the ruleset applied at start is the one applied before")
	assert.Equal(t, peers, tun2.peers, "the tunnel was given the peers again")
}

func TestManager_LoadWithNoStateClosesTheHost(t *testing.T) {
	f := &fakeRunner{}
	m := newManager(f, &fakeTunnel{}, filepath.Join(t.TempDir(), "networks.json"))
	require.NoError(t, m.Load(context.Background()))
	require.Len(t, f.calls, 1, "the empty ruleset is applied")
	assert.Contains(t, f.last(), "policy drop")

	bad := filepath.Join(t.TempDir(), "networks.json")
	require.NoError(t, os.WriteFile(bad, []byte("{"), 0o600))
	assert.Error(t, newManager(&fakeRunner{}, &fakeTunnel{}, bad).Load(context.Background()))
}

func TestManager_AFailedWriteOfTheStateUndoesTheChange(t *testing.T) {
	// The state's directory is a file, so it cannot be written.
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	f := &fakeRunner{}
	m := newManager(f, &fakeTunnel{}, filepath.Join(blocker, "networks.json"))
	err := m.Declare(context.Background(), network.Network{Name: "a", Prefix: pfx("10.8.0.0/16")})
	assert.Error(t, err)
	assert.Empty(t, m.Networks())
	assert.NotContains(t, f.last(), "net_a", "the host runs the state it had")
}

func TestLoadKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hostd", "wg.key")
	key, err := network.LoadKey(path)
	require.NoError(t, err)
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	dst, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dst.Mode().Perm())
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, base64.StdEncoding.EncodeToString(key[:])+"\n", string(raw))

	again, err := network.LoadKey(path)
	require.NoError(t, err)
	assert.Equal(t, key, again, "a second start keeps the key")

	other, err := network.LoadKey(filepath.Join(dir, "other.key"))
	require.NoError(t, err)
	assert.NotEqual(t, key, other, "each host makes a key of its own")
	_, err = wgtypes.NewKey(other[:])
	assert.NoError(t, err)
}

func TestLoadKey_Refusals(t *testing.T) {
	dir := t.TempDir()
	open := filepath.Join(dir, "open.key")
	require.NoError(t, os.WriteFile(open, []byte("x\n"), 0o640))
	_, err := network.LoadKey(open)
	assert.ErrorContains(t, err, "0600", "a key others can read is refused")

	junk := filepath.Join(dir, "junk.key")
	require.NoError(t, os.WriteFile(junk, []byte("secret-looking-junk\n"), 0o600))
	_, err = network.LoadKey(junk)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-looking-junk", "an error never carries the file's contents")
}

func TestManager_ADeclarationThatChangesNothingReplacesNoTable(t *testing.T) {
	f, tun := &fakeRunner{}, &fakeTunnel{}
	m := newManager(f, tun, "")
	ctx := context.Background()
	n := network.Network{Name: "a", Prefix: pfx("10.8.0.0/16"), Remote: []netip.Prefix{pfx("10.0.2.7/32"), pfx("10.0.2.8/32")}}
	peers := []network.Peer{{PublicKey: "KEYONE", Prefixes: []netip.Prefix{pfx("10.8.2.0/24"), pfx("10.8.3.0/24")}}}
	require.NoError(t, m.Declare(ctx, n))
	require.NoError(t, m.SetPeers(ctx, peers))
	before := len(f.calls)

	// The same, in another order: the table's counters are not reset by it.
	n.Remote = []netip.Prefix{pfx("10.0.2.8/32"), pfx("10.0.2.7/32")}
	peers[0].Prefixes = []netip.Prefix{pfx("10.8.3.0/24"), pfx("10.8.2.0/24")}
	require.NoError(t, m.Declare(ctx, n))
	require.NoError(t, m.SetPeers(ctx, peers))
	assert.Len(t, f.calls, before, "nothing changed, so nft ran no more")

	n.Remote = n.Remote[:1]
	require.NoError(t, m.Declare(ctx, n))
	assert.Len(t, f.calls, before+1, "a change is applied")
}
