package dhcpserver_test

import (
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/dhcp4"
)

var (
	broadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	bcastIP   = netip.AddrFrom4([4]byte{255, 255, 255, 255})
)

// message is a client's DHCP message of the type, with the options added, as the bytes of a payload.
func message(mac net.HardwareAddr, t dhcp4.MessageType, extra map[byte][]byte) []byte {
	m := dhcp4.Message{XID: 0x1234, CHAddr: mac, Options: map[byte][]byte{dhcp4.OptMessageType: {byte(t)}}}
	for k, v := range extra {
		m.Options[k] = v
	}
	return m.Encode()
}

// clientFrame is the Ethernet frame a client puts on the wire for the UDP payload from port 68 to 67.
func clientFrame(src, dst net.HardwareAddr, from, to netip.Addr, payload []byte) []byte {
	b := make([]byte, 14+20+8+len(payload))
	copy(b[0:], dst)
	copy(b[6:], src)
	binary.BigEndian.PutUint16(b[12:], 0x0800)
	ip := b[14:34]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(20+8+len(payload)))
	ip[8], ip[9] = 64, 17
	f, d := from.As4(), to.As4()
	copy(ip[12:], f[:])
	copy(ip[16:], d[:])
	binary.BigEndian.PutUint16(ip[10:], sum(ip))
	udp := b[34:]
	binary.BigEndian.PutUint16(udp[0:], 68)
	binary.BigEndian.PutUint16(udp[2:], 67)
	binary.BigEndian.PutUint16(udp[4:], uint16(8+len(payload)))
	copy(udp[8:], payload)
	return b
}

// arpRequest is the frame of a host that asks who has target.
func arpRequest(mac net.HardwareAddr, from, target netip.Addr) []byte {
	b := make([]byte, 60)
	copy(b[0:], broadcast)
	copy(b[6:], mac)
	binary.BigEndian.PutUint16(b[12:], 0x0806)
	a := b[14:]
	binary.BigEndian.PutUint16(a[0:], 1)
	binary.BigEndian.PutUint16(a[2:], 0x0800)
	a[4], a[5] = 6, 4
	binary.BigEndian.PutUint16(a[6:], 1)
	copy(a[8:], mac)
	f, tg := from.As4(), target.As4()
	copy(a[14:], f[:])
	copy(a[24:], tg[:])
	return b
}

// parsed is what a frame from the server says, read from the bytes.
type parsed struct {
	dstMAC, srcMAC net.HardwareAddr
	srcIP, dstIP   netip.Addr
	msg            dhcp4.Message
}

// parse reads the frame as the client's kernel would: the IPv4 header's checksum must be right and the
// ports must be 67 to 68, or the test fails.
func parse(t *testing.T, b []byte) parsed {
	t.Helper()
	require.GreaterOrEqual(t, len(b), 14+20+8)
	require.Equal(t, uint16(0x0800), binary.BigEndian.Uint16(b[12:]))
	ip := b[14:34]
	require.Equal(t, byte(0x45), ip[0])
	require.Equal(t, uint16(0), sum(ip), "the IPv4 header checksum is valid")
	total := int(binary.BigEndian.Uint16(ip[2:]))
	require.Equal(t, len(b)-14, total)
	require.Equal(t, byte(17), ip[9])
	udp := b[34:]
	require.Equal(t, uint16(67), binary.BigEndian.Uint16(udp[0:]))
	require.Equal(t, uint16(68), binary.BigEndian.Uint16(udp[2:]))
	require.Equal(t, total-20, int(binary.BigEndian.Uint16(udp[4:])))
	m, err := dhcp4.Decode(udp[8:])
	require.NoError(t, err)
	require.True(t, m.Reply)
	return parsed{
		dstMAC: net.HardwareAddr(b[0:6]), srcMAC: net.HardwareAddr(b[6:12]),
		srcIP: netip.AddrFrom4([4]byte(ip[12:16])), dstIP: netip.AddrFrom4([4]byte(ip[16:20])), msg: m,
	}
}

// udpTo68 is the DHCP payload of a frame to the client mac, as its kernel hands it to the client.
func udpTo68(t *testing.T, b []byte, mac net.HardwareAddr) []byte {
	t.Helper()
	f := parse(t, b)
	require.Equal(t, mac, f.dstMAC)
	return b[14+20+8 : 14+int(binary.BigEndian.Uint16(b[16:]))]
}

func sum(b []byte) uint16 {
	var s uint32
	for ; len(b) >= 2; b = b[2:] {
		s += uint32(binary.BigEndian.Uint16(b))
	}
	for s > 0xffff {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

// pipe is a Frames of channels: in is what the segment carries to the server, out what the server puts
// on it.
type pipe struct {
	in, out chan []byte
	once    sync.Once
	closed  chan struct{}
}

func newPipe() *pipe {
	return &pipe{in: make(chan []byte, 4), out: make(chan []byte, 4), closed: make(chan struct{})}
}

func (p *pipe) Read() ([]byte, error) {
	select {
	case b := <-p.in:
		return b, nil
	case <-p.closed:
		return nil, net.ErrClosed
	}
}

func (p *pipe) Write(b []byte) error {
	p.out <- b
	return nil
}

func (p *pipe) Close() error {
	p.once.Do(func() { close(p.closed) })
	return nil
}
