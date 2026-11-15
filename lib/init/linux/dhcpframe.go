package linux

import (
	"encoding/binary"
	"errors"
	"net"
	"net/netip"

	"github.com/garamsh/naru/lib/init/dhcp4"
)

// The headers of a DHCP packet sent before the host has an address, which only a packet socket can
// carry: Ethernet, IPv4 and UDP.
const (
	etherLen    = 14
	ipv4Len     = 20
	udpLen      = 8
	etherTypeIP = 0x0800
	protoUDP    = 17
)

var broadcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// buildFrame returns the Ethernet frame that carries payload from the client's port to the
// server's, to the broadcast MAC. src is the zero Addr before the client has an address, and dst
// is the zero Addr for the broadcast address.
func buildFrame(srcMAC net.HardwareAddr, src, dst netip.Addr, payload []byte) []byte {
	if !src.IsValid() {
		src = netip.IPv4Unspecified()
	}
	if !dst.IsValid() {
		dst = netip.AddrFrom4([4]byte{255, 255, 255, 255})
	}
	b := make([]byte, etherLen+ipv4Len+udpLen+len(payload))
	copy(b[0:6], broadcastMAC)
	copy(b[6:12], srcMAC)
	binary.BigEndian.PutUint16(b[12:], etherTypeIP)

	ip := b[etherLen : etherLen+ipv4Len]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(ipv4Len+udpLen+len(payload)))
	ip[8], ip[9] = 64, protoUDP
	s, d := src.As4(), dst.As4()
	copy(ip[12:], s[:])
	copy(ip[16:], d[:])
	binary.BigEndian.PutUint16(ip[10:], checksum(ip))

	udp := b[etherLen+ipv4Len:]
	binary.BigEndian.PutUint16(udp[0:], dhcp4.ClientPort)
	binary.BigEndian.PutUint16(udp[2:], dhcp4.ServerPort)
	binary.BigEndian.PutUint16(udp[4:], uint16(udpLen+len(payload)))
	copy(udp[udpLen:], payload) // the UDP checksum stays 0: RFC 768 lets IPv4 omit it
	return b
}

var errNotDHCP = errors.New("not a DHCP reply")

// parseFrame returns the UDP payload of a frame that is an unfragmented IPv4 packet from the
// DHCP server port to the client port, with a valid IPv4 header checksum.
func parseFrame(b []byte) ([]byte, error) {
	if len(b) < etherLen+ipv4Len+udpLen || binary.BigEndian.Uint16(b[12:]) != etherTypeIP {
		return nil, errNotDHCP
	}
	ip := b[etherLen:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ihl < ipv4Len || len(ip) < ihl+udpLen || ip[9] != protoUDP {
		return nil, errNotDHCP
	}
	if binary.BigEndian.Uint16(ip[6:])&0x3fff != 0 {
		return nil, errors.New("a fragmented packet")
	}
	if checksum(ip[:ihl]) != 0 {
		return nil, errors.New("the IPv4 header checksum is wrong")
	}
	total := int(binary.BigEndian.Uint16(ip[2:]))
	if total < ihl+udpLen || total > len(ip) {
		return nil, errors.New("the IPv4 length is wrong")
	}
	udp := ip[ihl:total]
	if binary.BigEndian.Uint16(udp[0:]) != dhcp4.ServerPort || binary.BigEndian.Uint16(udp[2:]) != dhcp4.ClientPort {
		return nil, errNotDHCP
	}
	n := int(binary.BigEndian.Uint16(udp[4:]))
	if n < udpLen || n > len(udp) {
		return nil, errors.New("the UDP length is wrong")
	}
	return udp[udpLen:n], nil
}

// checksum is the Internet checksum (RFC 1071) of b. Over a header that carries its own checksum
// it is 0.
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
