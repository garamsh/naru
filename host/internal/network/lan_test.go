package network_test

import (
	"net"
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
