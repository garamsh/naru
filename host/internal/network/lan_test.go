package network_test

import (
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/network"
)

func TestARPAnnouncement_IsAnUnsolicitedReplyForTheAddress(t *testing.T) {
	hw := net.HardwareAddr{0x02, 0, 0, 0xaa, 0xbb, 0xcc}
	got, err := network.ARPAnnouncement(hw, addr("192.168.0.80"))
	require.NoError(t, err)
	want := []byte{
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, // destination: broadcast
		0x02, 0, 0, 0xaa, 0xbb, 0xcc, // source: the interface
		0x08, 0x06, // ARP
		0, 1, 0x08, 0, 6, 4, // Ethernet, IPv4
		0, 2, // reply
		0x02, 0, 0, 0xaa, 0xbb, 0xcc, 192, 168, 0, 80, // sender: the interface owns the address
		0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 192, 168, 0, 80, // target: the same address
	}
	assert.Equal(t, want, got)

	_, err = network.ARPAnnouncement(hw, addr("fd00::1"))
	assert.Error(t, err, "IPv4 only")
	_, err = network.ARPAnnouncement(net.HardwareAddr{1, 2}, addr("192.168.0.80"))
	assert.Error(t, err)
}

func TestRefuseHostAddress(t *testing.T) {
	lan := netip.MustParsePrefix("192.168.0.7/24")
	held := []network.HostAddress{
		{Prefix: lan, OnLAN: true},
		{Prefix: netip.MustParsePrefix("10.1.1.1/8")},
		{Prefix: netip.MustParsePrefix("192.168.0.80/32"), OnLAN: true, Ours: true},
	}
	gateways := []netip.Addr{addr("192.168.0.1")}
	for _, tc := range []struct {
		name string
		a    string
		want bool
	}{
		{"an address of the host on the LAN", "192.168.0.7", true},
		{"an address of the host elsewhere", "10.1.1.1", true},
		{"the network address of the LAN", "192.168.0.0", true},
		{"the broadcast address of the LAN", "192.168.0.255", true},
		{"a default gateway", "192.168.0.1", true},
		{"control: a free address of the LAN", "192.168.0.50", false},
		{"control: an address a lease holds is ours to renew", "192.168.0.80", false},
		{"control: the network address of another interface's network", "10.0.0.0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := network.RefuseHostAddress(addr(tc.a), held, gateways)
			if tc.want {
				assert.ErrorIs(t, err, network.ErrHostAddress)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
