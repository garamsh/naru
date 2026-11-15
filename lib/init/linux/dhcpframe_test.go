package linux

import (
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/dhcp4"
)

// reply turns a frame the client built into one a server would send back: the ports swapped.
func reply(frame []byte) []byte {
	out := append([]byte(nil), frame...)
	udp := out[etherLen+ipv4Len:]
	udp[0], udp[1], udp[2], udp[3] = 0, dhcp4.ServerPort, 0, dhcp4.ClientPort
	return out
}

// fixChecksum makes the IPv4 header of b valid again after a test changed it.
func fixChecksum(b []byte) []byte {
	ip := b[etherLen : etherLen+ipv4Len]
	ip[10], ip[11] = 0, 0
	c := checksum(ip)
	ip[10], ip[11] = byte(c>>8), byte(c)
	return b
}

func TestBuildFrame_CarriesThePayloadFromTheClientPortToTheServerPortWithAValidHeader(t *testing.T) {
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 9}

	f := buildFrame(mac, netip.Addr{}, netip.Addr{}, []byte("payload"))

	assert.Equal(t, broadcastMAC, net.HardwareAddr(f[0:6]))
	assert.Equal(t, mac, net.HardwareAddr(f[6:12]))
	assert.Equal(t, uint16(0), checksum(f[etherLen:etherLen+ipv4Len]), "the IPv4 header checksum holds")
	assert.Equal(t, []byte{255, 255, 255, 255}, f[etherLen+16:etherLen+20], "the zero destination is the broadcast address")
	assert.Equal(t, []byte{0, 0, 0, 0}, f[etherLen+12:etherLen+16], "the zero source is 0.0.0.0")
	assert.Equal(t, []byte("payload"), f[etherLen+ipv4Len+udpLen:])
}

func TestParseFrame_ReturnsThePayloadOfAServerReply(t *testing.T) {
	f := reply(buildFrame(net.HardwareAddr{2, 0, 0, 0, 0, 9}, netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), []byte("payload")))

	got, err := parseFrame(f)

	require.NoError(t, err)
	assert.Equal(t, []byte("payload"), got)
}

func TestParseFrame_Refuses(t *testing.T) {
	good := reply(buildFrame(net.HardwareAddr{2, 0, 0, 0, 0, 9}, netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), []byte("payload")))
	_, err := parseFrame(good)
	require.NoError(t, err, "the control: the unmodified frame parses")
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"a frame shorter than its headers", func(b []byte) []byte { return b[:20] }},
		{"a frame that is not IPv4", func(b []byte) []byte { b[12], b[13] = 0x86, 0xdd; return b }},
		{"a bad IPv4 header checksum", func(b []byte) []byte { b[etherLen+8] ^= 0xff; return b }},
		{"a fragment", func(b []byte) []byte { b[etherLen+6] = 0x20; return fixChecksum(b) }},
		{"a packet to another port", func(b []byte) []byte { b[etherLen+ipv4Len+3] = 99; return b }},
		{"a packet from another port", func(b []byte) []byte { b[etherLen+ipv4Len+1] = 99; return b }},
		{"a UDP length past the packet", func(b []byte) []byte { b[etherLen+ipv4Len+5] = 0xff; return b }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseFrame(tt.mutate(append([]byte(nil), good...)))

			assert.Error(t, err)
		})
	}
}
