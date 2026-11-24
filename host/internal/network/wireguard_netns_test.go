//go:build netns

package network_test

import (
	"net/netip"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/garamsh/naru/host/internal/network"
)

// linkAddrs is the IPv4 addresses on the interface name.
func linkAddrs(t *testing.T, name string) []netip.Prefix {
	t.Helper()
	l, err := netlink.LinkByName(name)
	require.NoError(t, err)
	as, err := netlink.AddrList(l, netlink.FAMILY_V4)
	require.NoError(t, err)
	var out []netip.Prefix
	for _, a := range as {
		ip, _ := netip.AddrFromSlice(a.IP)
		ones, _ := a.Mask.Size()
		out = append(out, netip.PrefixFrom(ip.Unmap(), ones))
	}
	return out
}

// linkRoutes is the destinations routed through the interface name.
func linkRoutes(t *testing.T, name string) []netip.Prefix {
	t.Helper()
	l, err := netlink.LinkByName(name)
	require.NoError(t, err)
	rs, err := netlink.RouteList(l, netlink.FAMILY_V4)
	require.NoError(t, err)
	var out []netip.Prefix
	for _, r := range rs {
		if r.Dst == nil {
			continue
		}
		ip, _ := netip.AddrFromSlice(r.Dst.IP)
		ones, _ := r.Dst.Mask.Size()
		out = append(out, netip.PrefixFrom(ip.Unmap(), ones))
	}
	return out
}

func TestWireGuard_TheMeshAddressIsTheInterfacesOnlyAddressAndAPeersIsAllowedAndRouted(t *testing.T) {
	w, err := network.NewWireGuard("wgmesh", filepath.Join(t.TempDir(), "wg.key"), 51999)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = w.Close()
		if l, err := netlink.LinkByName("wgmesh"); err == nil {
			_ = netlink.LinkDel(l)
		}
	})
	other, err := wgtypes.GeneratePrivateKey()
	require.NoError(t, err)
	peer := network.Peer{Name: "h2", PublicKey: other.PublicKey().String(), Prefixes: []netip.Prefix{netip.MustParsePrefix("10.1.2.0/24")}, Mesh: netip.MustParseAddr("10.99.0.2")}

	require.NoError(t, w.Set(netip.MustParseAddr("10.99.0.1"), []network.Peer{peer}))
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.99.0.1/32")}, linkAddrs(t, "wgmesh"))
	assert.ElementsMatch(t, []netip.Prefix{netip.MustParsePrefix("10.1.2.0/24"), netip.MustParsePrefix("10.99.0.2/32")}, linkRoutes(t, "wgmesh"),
		"the peer's mesh address is routed to the interface beside its prefixes")
	c, err := wgctrl.New()
	require.NoError(t, err)
	defer c.Close()
	dev, err := c.Device("wgmesh")
	require.NoError(t, err)
	require.Len(t, dev.Peers, 1)
	var allowed []string
	for _, a := range dev.Peers[0].AllowedIPs {
		allowed = append(allowed, a.String())
	}
	assert.ElementsMatch(t, []string{"10.1.2.0/24", "10.99.0.2/32"}, allowed, "the peer's mesh address is one of its allowed IPs")

	require.NoError(t, w.Set(netip.MustParseAddr("10.99.0.3"), []network.Peer{peer}))
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.99.0.3/32")}, linkAddrs(t, "wgmesh"), "a new mesh address replaces the old")

	peer.Mesh = netip.Addr{}
	require.NoError(t, w.Set(netip.Addr{}, []network.Peer{peer}))
	assert.Empty(t, linkAddrs(t, "wgmesh"), "no mesh address leaves the interface with none")
	assert.Equal(t, []netip.Prefix{netip.MustParsePrefix("10.1.2.0/24")}, linkRoutes(t, "wgmesh"), "and a peer with none is routed only its prefixes")
}
