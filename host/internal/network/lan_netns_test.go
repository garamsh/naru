//go:build netns

package network_test

// Run under unshare --map-root-user --net:
//
//	unshare --map-root-user --net \
//	  go test -v -tags netns -run . \
//	  github.com/garamsh/naru/host/internal/network

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/garamsh/naru/host/internal/network"
)

// TestMain brings up the loopback interface, which is down in the fresh network
// namespace that 'unshare --map-root-user --net' created. Without it the kernel
// cannot deliver packets to local addresses (the proxy tests need this).
// When running as root inside a QEMU guest, it first creates a private network
// namespace so the test's interfaces do not collide with the host's.
func TestMain(m *testing.M) {
	if os.Getuid() == 0 {
		// In a QEMU guest (real root, no prior unshare) create an isolated netns.
		// Under 'unshare --map-root-user --net' we are already root (mapped) and
		// already in a fresh netns, so skip the extra Unshare there.
		if !inFreshNetns() {
			if err := unix.Unshare(unix.CLONE_NEWNET); err != nil {
				fmt.Fprintf(os.Stderr, "CLONE_NEWNET: %v\n", err)
				os.Exit(1)
			}
		}
	}
	// Loopback is down in a fresh netns; bring it up for local-address routing.
	if lo, err := netlink.LinkByName("lo"); err == nil {
		_ = netlink.LinkSetUp(lo)
	}
	os.Exit(m.Run())
}

// inFreshNetns reports whether the process is already in a network namespace
// that has no interfaces other than loopback — the sign of 'unshare --net'.
func inFreshNetns() bool {
	links, err := netlink.LinkList()
	if err != nil {
		return false
	}
	for _, l := range links {
		if l.Attrs().Name != "lo" {
			return false
		}
	}
	return true
}

// setupVeth creates a named veth pair in the test's netns, brings up the first
// end, assigns a /24 to it, and returns the link and a LAN bound to it.
func setupVeth(t *testing.T, name, peerName, cidr string) (netlink.Link, *network.LAN) {
	t.Helper()
	veth := &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: name}, PeerName: peerName}
	require.NoError(t, netlink.LinkAdd(veth))
	link, err := netlink.LinkByName(name)
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(link))
	sub, err := netlink.ParseAddr(cidr)
	require.NoError(t, err)
	require.NoError(t, netlink.AddrAdd(link, sub))
	lan, err := network.NewLAN(name)
	require.NoError(t, err)
	return link, lan
}

// validLftOf returns the ValidLft of addr on link, or -1 when absent.
func validLftOf(t *testing.T, link netlink.Link, addr netip.Addr) int {
	t.Helper()
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	require.NoError(t, err)
	for _, x := range addrs {
		if ip, ok := netip.AddrFromSlice(x.IP); ok && ip.Unmap() == addr {
			return x.ValidLft
		}
	}
	return -1
}

// TestLAN_AddResetsTheKernelLifetimeOnRenewal proves that LAN.Add resets the
// kernel's valid_lft on every call. RTM_NEWADDR|NLM_F_REPLACE (AddrReplace)
// resets valid_lft on the production kernel 6.18.63 (confirmed on QEMU). The
// test fails without the renewal: valid_lft does not rise after the second Add,
// and the address expires on the original countdown.
//
// A 60-second lifetime tolerates CPU stalls on a loaded QEMU guest: polling to
// the half-way point (≈30 s) leaves ≈28 s of stall tolerance in the final
// survival check.
func TestLAN_AddResetsTheKernelLifetimeOnRenewal(t *testing.T) {
	const lft = 60 * time.Second
	leased := netip.MustParseAddr("10.9.0.80")
	link, lan := setupVeth(t, "veth0", "veth1", "10.9.0.1/24")

	// Initial lease.
	require.NoError(t, lan.Add(leased, lft))
	initial := validLftOf(t, link, leased)
	require.Greater(t, initial, 0, "address must be on the interface after the first Add")

	// Poll until the countdown has reached the half-way point; no fixed sleep.
	var before int
	require.Eventually(t, func() bool {
		before = validLftOf(t, link, leased)
		return before > 0 && before <= initial/2
	}, lft, time.Second, "valid_lft must count down to half within the lease lifetime")
	assert.Less(t, before, initial, "valid_lft must have counted down")

	// Renewal.
	require.NoError(t, lan.Add(leased, lft))
	after := validLftOf(t, link, leased)
	require.Greater(t, after, 0, "address must still be present after renewal")

	// A reset lifetime is strictly greater than the pre-renewal remainder; a
	// kernel that ignores the update would show after <= before.
	assert.Greater(t, after, before,
		"valid_lft must be reset by the renewal (before=%d, after=%d)", before, after)

	// Verify the address survives past the pre-renewal expiry. If valid_lft was
	// not reset, the address expires within before seconds; with a reset it
	// lives for a full lft from the renewal point.
	require.Never(t, func() bool {
		return validLftOf(t, link, leased) <= 0
	}, time.Duration(before+2)*time.Second, time.Second,
		"address must survive past the original expiry, proving the lifetime was reset")
}

// TestLAN_RenewalSurvivesAProxyListenerAndEstablishedConnection shows that
// LAN.Add (AddrReplace) does not break a TCP listener or an established
// connection that uses the leased address. The listener is on the leased /32;
// the client connects to it through the kernel's local-address routing, which
// requires no separate network path.
func TestLAN_RenewalSurvivesAProxyListenerAndEstablishedConnection(t *testing.T) {
	const (
		lft  = 30 * time.Second
		port = "9877"
	)
	leased := netip.MustParseAddr("10.9.2.80")
	_, lan := setupVeth(t, "veth2", "veth3", "10.9.2.1/24")

	// Initial lease.
	require.NoError(t, lan.Add(leased, lft))

	addr := net.JoinHostPort(leased.String(), port)

	// (a) Open a listener on the leased address.
	ln, err := net.Listen("tcp4", addr)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	// Establish a connection before the renewal.
	accept := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accept <- nil
		} else {
			accept <- c
		}
	}()
	client, err := net.DialTimeout("tcp4", addr, 5*time.Second)
	require.NoError(t, err, "initial dial must succeed")
	t.Cleanup(func() { _ = client.Close() })
	server := <-accept
	require.NotNil(t, server, "initial accept must succeed")
	t.Cleanup(func() { _ = server.Close() })

	// Confirm the connection works before the renewal.
	require.NoError(t, client.SetDeadline(time.Now().Add(time.Second)))
	_, err = client.Write([]byte("ping"))
	require.NoError(t, err, "client write must succeed before renewal")
	buf := make([]byte, 4)
	require.NoError(t, server.SetDeadline(time.Now().Add(time.Second)))
	_, err = server.Read(buf)
	require.NoError(t, err, "server read must succeed before renewal")

	// Renewal.
	require.NoError(t, lan.Add(leased, lft))

	// (a) The listener must still accept new connections after the renewal.
	accept2 := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			accept2 <- nil
		} else {
			accept2 <- c
		}
	}()
	client2, err := net.DialTimeout("tcp4", addr, 5*time.Second)
	require.NoError(t, err, "dial after renewal must succeed — listener must survive Del+Add")
	t.Cleanup(func() { _ = client2.Close() })
	server2 := <-accept2
	require.NotNil(t, server2, "accept after renewal must succeed")
	t.Cleanup(func() { _ = server2.Close() })

	// (b) The established connection must still carry traffic after the renewal.
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	_, errW := client.Write([]byte("pong"))
	_ = server.SetDeadline(time.Now().Add(2 * time.Second))
	_, errR := server.Read(buf)
	assert.NoError(t, errW, "established connection: client write must survive renewal")
	assert.NoError(t, errR, "established connection: server read must survive renewal")
}
