package dhcpserver

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"

	"github.com/garamsh/naru/lib/init/dhcp4"
)

const (
	etherLen     = 14
	ipv4Len      = 20
	udpLen       = 8
	arpLen       = 28
	etherTypeIP  = 0x0800
	etherTypeARP = 0x0806
	protoUDP     = 17
	arpRequest   = 1
	arpReplyOp   = 2
	minFrame     = 60
)

var broadcastMAC = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

// udpPayloadTo is the UDP payload of an IPv4 frame addressed to port, or nil when the frame is not
// one. The IPv4 header's checksum is not checked: a frame that crossed QEMU's socket is intact.
func udpPayloadTo(frame []byte, port uint16) []byte {
	if len(frame) < etherLen+ipv4Len+udpLen {
		return nil
	}
	ip := frame[etherLen:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ihl < ipv4Len || len(ip) < ihl+udpLen || ip[9] != protoUDP {
		return nil
	}
	total := int(binary.BigEndian.Uint16(ip[2:]))
	if total < ihl+udpLen || total > len(ip) {
		return nil
	}
	udp := ip[ihl:total]
	if binary.BigEndian.Uint16(udp[2:]) != port {
		return nil
	}
	n := int(binary.BigEndian.Uint16(udp[4:]))
	if n < udpLen || n > len(udp) {
		return nil
	}
	return udp[udpLen:n]
}

// arpReply answers an ARP request for the server's address with the server's MAC.
func (s *Server) arpReply(frame []byte) []byte {
	arp := frame[etherLen:]
	if len(arp) < arpLen || binary.BigEndian.Uint16(arp[0:]) != 1 || binary.BigEndian.Uint16(arp[2:]) != etherTypeIP ||
		arp[4] != 6 || arp[5] != 4 || binary.BigEndian.Uint16(arp[6:]) != arpRequest {
		return nil
	}
	own := s.cfg.Address.As4()
	if !bytes.Equal(arp[24:28], own[:]) {
		return nil
	}
	asker := net.HardwareAddr(arp[8:14])
	out := make([]byte, etherLen+arpLen, minFrame)
	copy(out[0:], asker)
	copy(out[6:], s.cfg.MAC)
	binary.BigEndian.PutUint16(out[12:], etherTypeARP)
	a := out[etherLen:]
	binary.BigEndian.PutUint16(a[0:], 1)
	binary.BigEndian.PutUint16(a[2:], etherTypeIP)
	a[4], a[5] = 6, 4
	binary.BigEndian.PutUint16(a[6:], arpReplyOp)
	copy(a[8:], s.cfg.MAC)
	copy(a[14:], own[:])
	copy(a[18:], asker)
	copy(a[24:], arp[14:18])
	return out[:minFrame]
}

// dhcpFrame is the Ethernet frame that carries out, the server's answer to req, to the client's MAC. The
// IPv4 destination is the client's own address when it renews, and the broadcast address otherwise: a
// client that has no address yet reads frames addressed to its MAC whatever the IPv4 destination.
func (s *Server) dhcpFrame(req, out dhcp4.Message) []byte {
	dst := netip.AddrFrom4([4]byte{255, 255, 255, 255})
	if req.CIAddr.IsValid() && !req.CIAddr.IsUnspecified() && out.Type() != dhcp4.Nak {
		dst = req.CIAddr
	}
	payload := out.Encode()
	b := make([]byte, etherLen+ipv4Len+udpLen+len(payload))
	copy(b[0:], req.CHAddr)
	copy(b[6:], s.cfg.MAC)
	binary.BigEndian.PutUint16(b[12:], etherTypeIP)
	ip := b[etherLen : etherLen+ipv4Len]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(ipv4Len+udpLen+len(payload)))
	ip[8], ip[9] = 64, protoUDP
	src, d := s.cfg.Address.As4(), dst.As4()
	copy(ip[12:], src[:])
	copy(ip[16:], d[:])
	binary.BigEndian.PutUint16(ip[10:], checksum(ip))
	udp := b[etherLen+ipv4Len:]
	binary.BigEndian.PutUint16(udp[0:], dhcp4.ServerPort)
	binary.BigEndian.PutUint16(udp[2:], dhcp4.ClientPort)
	binary.BigEndian.PutUint16(udp[4:], uint16(udpLen+len(payload)))
	copy(udp[udpLen:], payload) // the UDP checksum stays 0: RFC 768 lets IPv4 omit it
	return b
}

// checksum is the Internet checksum (RFC 1071) of b.
func checksum(b []byte) uint16 {
	var sum uint32
	for ; len(b) >= 2; b = b[2:] {
		sum += uint32(binary.BigEndian.Uint16(b))
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
