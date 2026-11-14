// Package pcap reads the capture QEMU's filter-dump writes (the pcap format, Ethernet frames) and
// says what crossed the link: the network tests show that the link between two hosts carries only
// WireGuard, which is UDP to the tunnel port.
package pcap

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
)

// Packet is one IPv4 packet of the capture.
type Packet struct {
	Src, Dst netip.Addr
	// Proto is the IP protocol number: 17 is UDP, 6 is TCP.
	Proto            uint8
	SrcPort, DstPort uint16
}

// Capture is what a file held.
type Capture struct {
	// Frames is the number of frames; IPv4 are the IPv4 packets among them.
	Frames int
	IPv4   []Packet
	// Raw is the file's bytes, for a test to look for a payload in.
	Raw []byte
}

// Read parses the capture in file.
func Read(file string) (Capture, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return Capture{}, err
	}
	return Parse(b)
}

// Parse parses a capture. A frame cut off at the end, where QEMU was killed mid-write, ends the
// capture without an error.
func Parse(b []byte) (Capture, error) {
	c := Capture{Raw: b}
	if len(b) < 24 {
		return c, errors.New("the capture has no pcap header")
	}
	var order binary.ByteOrder
	switch {
	case bytes.Equal(b[:4], []byte{0xd4, 0xc3, 0xb2, 0xa1}), bytes.Equal(b[:4], []byte{0x4d, 0x3c, 0xb2, 0xa1}):
		order = binary.LittleEndian
	case bytes.Equal(b[:4], []byte{0xa1, 0xb2, 0xc3, 0xd4}), bytes.Equal(b[:4], []byte{0xa1, 0xb2, 0x3c, 0x4d}):
		order = binary.BigEndian
	default:
		return c, fmt.Errorf("not a pcap file: magic % x", b[:4])
	}
	if lt := order.Uint32(b[20:]); lt != 1 {
		return c, fmt.Errorf("the link type is %d, not Ethernet", lt)
	}
	for off := 24; off+16 <= len(b); {
		n := int(order.Uint32(b[off+8:]))
		off += 16
		if n < 0 || off+n > len(b) {
			break
		}
		c.Frames++
		if p, ok := ipv4(b[off : off+n]); ok {
			c.IPv4 = append(c.IPv4, p)
		}
		off += n
	}
	return c, nil
}

func ipv4(f []byte) (Packet, bool) {
	if len(f) < 34 || f[12] != 0x08 || f[13] != 0x00 {
		return Packet{}, false
	}
	ip := f[14:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ihl < 20 || len(ip) < ihl {
		return Packet{}, false
	}
	p := Packet{Src: netip.AddrFrom4([4]byte(ip[12:16])), Dst: netip.AddrFrom4([4]byte(ip[16:20])), Proto: ip[9]}
	if (p.Proto == 17 || p.Proto == 6) && len(ip) >= ihl+4 {
		p.SrcPort, p.DstPort = binary.BigEndian.Uint16(ip[ihl:]), binary.BigEndian.Uint16(ip[ihl+2:])
	}
	return p, true
}

// Between returns the packets whose source and destination are both in addrs: the traffic of those
// hosts with each other.
func (c Capture) Between(addrs []netip.Addr) []Packet {
	var out []Packet
	for _, p := range c.IPv4 {
		if slices.Contains(addrs, p.Src) && slices.Contains(addrs, p.Dst) {
			out = append(out, p)
		}
	}
	return out
}

// IsWireGuard reports whether p is UDP to or from port.
func (p Packet) IsWireGuard(port uint16) bool {
	return p.Proto == 17 && (p.DstPort == port || p.SrcPort == port)
}
