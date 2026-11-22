//go:build netns

package network_test

// Run under unshare --map-root-user --net:
//
//	unshare --map-root-user --net \
//	  go test -v -tags netns -run TestLAN_AddResetsTheKernelLifetimeOnRenewal \
//	  github.com/garamsh/naru/host/internal/network

import (
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/garamsh/naru/host/internal/network"
)

// TestLAN_AddResetsTheKernelLifetimeOnRenewal proves that LAN.Add resets the
// kernel's valid_lft on every call. Without the fix, RTM_NEWADDR|NLM_F_REPLACE
// updates the address record but leaves the original countdown running, so a
// renewed lease expires on the kernel's first schedule rather than the latest
// one. The test must be run under unshare --map-root-user --net (-tags netns).
func TestLAN_AddResetsTheKernelLifetimeOnRenewal(t *testing.T) {
	const (
		ifname = "veth0"
		peer   = "veth1"
		lft    = 10 * time.Second
		half   = 6 * time.Second // sleep past half; countdown leaves ~4 s
	)
	leased := netip.MustParseAddr("10.9.0.80")

	// Create a veth pair in the test's (private) network namespace.
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: ifname}, PeerName: peer}
	require.NoError(t, netlink.LinkAdd(veth))
	link, err := netlink.LinkByName(ifname)
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(link))

	// Give the interface a /24 so it looks like a real LAN interface.
	sub, err := netlink.ParseAddr("10.9.0.1/24")
	require.NoError(t, err)
	require.NoError(t, netlink.AddrAdd(link, sub))

	lan, err := network.NewLAN(ifname)
	require.NoError(t, err)

	validLft := func() int {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
		require.NoError(t, err)
		for _, x := range addrs {
			if ip, ok := netip.AddrFromSlice(x.IP); ok && ip.Unmap() == leased {
				return x.ValidLft
			}
		}
		return -1 // absent
	}

	// Initial lease.
	require.NoError(t, lan.Add(leased, lft))
	initial := validLft()
	require.Greater(t, initial, 0, "address must be on the interface after the first Add")

	// Wait past the half-life; the kernel countdown should leave about 4 s.
	time.Sleep(half)
	before := validLft()
	require.Greater(t, before, 0, "address must still be present before renewal")
	assert.Less(t, before, initial, "valid_lft must have counted down")

	// Renewal with the same lifetime.
	require.NoError(t, lan.Add(leased, lft))
	after := validLft()
	require.Greater(t, after, 0, "address must still be present after renewal")

	// A reset lifetime is strictly greater than the pre-renewal remainder; a
	// kernel that ignores the update would show after <= before.
	assert.Greater(t, after, before,
		"valid_lft must be reset by the renewal (before=%d, after=%d)", before, after)

	// Wait out the original remaining time plus margin. If the renewal did not
	// reset the counter the address would have expired.
	time.Sleep(time.Duration(before+2) * time.Second)
	assert.Greater(t, validLft(), 0,
		"address must survive past the original expiry, proving the lifetime was reset")
}
