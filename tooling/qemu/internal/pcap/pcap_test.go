package pcap_test

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/pcap"
)

func frame(src, dst string, proto byte, sport, dport uint16) []byte {
	f := make([]byte, 14+20+8)
	f[12], f[13] = 0x08, 0x00
	ip := f[14:]
	ip[0], ip[9] = 0x45, proto
	s, d := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	copy(ip[12:], s[:])
	copy(ip[16:], d[:])
	binary.BigEndian.PutUint16(ip[20:], sport)
	binary.BigEndian.PutUint16(ip[22:], dport)
	return f
}

func file(frames ...[]byte) []byte {
	h := make([]byte, 24)
	binary.LittleEndian.PutUint32(h, 0xa1b2c3d4)
	binary.LittleEndian.PutUint32(h[20:], 1)
	for _, f := range frames {
		r := make([]byte, 16)
		binary.LittleEndian.PutUint32(r[8:], uint32(len(f)))
		binary.LittleEndian.PutUint32(r[12:], uint32(len(f)))
		h = append(append(h, r...), f...)
	}
	return h
}

func TestParse_ReadsIPv4PacketsAndSelectsTheTrafficBetweenTheHostsThatIsNotWireGuard(t *testing.T) {
	arp := make([]byte, 42)
	arp[12], arp[13] = 0x08, 0x06
	b := file(frame("10.78.0.1", "10.78.0.2", 17, 51820, 51820), frame("10.78.0.1", "10.78.0.2", 6, 40000, 7500), arp, frame("10.78.0.1", "8.8.8.8", 17, 1, 2))

	c, err := pcap.Parse(b[:len(b)-5]) // the last frame is cut off, as a killed QEMU leaves it
	require.NoError(t, err)
	assert.Equal(t, 3, c.Frames)
	between := c.Between([]netip.Addr{netip.MustParseAddr("10.78.0.1"), netip.MustParseAddr("10.78.0.2")})
	require.Len(t, between, 2)
	assert.True(t, between[0].IsWireGuard(51820))
	assert.False(t, between[1].IsWireGuard(51820), "a TCP packet to another port is not WireGuard")
}

func TestParse_RefusesWhatIsNotAnEthernetPcap(t *testing.T) {
	_, err := pcap.Parse([]byte("not a pcap file at all, but long enough"))
	assert.Error(t, err)
}
