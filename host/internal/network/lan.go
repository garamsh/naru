package network

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
)

// maxLabel is the longest label an IPv4 address takes: the length of an interface name.
const maxLabel = 15

const labelSuffix = ":naru"

// LAN is the Link of a Linux host: the interface name, with the addresses it holds as /32s that
// carry a label of ours, so a later run finds what an earlier one left.
type LAN struct {
	name  string
	label string
}

// NewLAN returns the Link of the interface name, which must exist. Its addresses are labelled
// name:naru, so the name is at most 10 characters.
func NewLAN(name string) (*LAN, error) {
	label := name + labelSuffix
	if len(label) > maxLabel {
		return nil, fmt.Errorf("the interface name %q is too long for the label %q of its addresses", name, label)
	}
	if _, err := netlink.LinkByName(name); err != nil {
		return nil, fmt.Errorf("the LAN interface %s: %w", name, err)
	}
	return &LAN{name: name, label: label}, nil
}

func (l *LAN) link() (netlink.Link, error) { return netlink.LinkByName(l.name) }

func hostAddr(a netip.Addr) *netlink.Addr {
	return &netlink.Addr{IPNet: &net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(32, 32)}}
}

// Add gives the interface the address for valid, which the kernel enforces: it removes the address
// when the time passes, whether or not hostd runs.
func (l *LAN) Add(a netip.Addr, valid time.Duration) error {
	link, err := l.link()
	if err != nil {
		return err
	}
	if err := l.check(a, false, false); err != nil {
		return err
	}
	x := hostAddr(a)
	x.Label = l.label
	secs := max(int((valid+time.Second-1)/time.Second), 1)
	x.ValidLft, x.PreferedLft = secs, secs
	return netlink.AddrReplace(link, x)
}

// Check refuses an address the host must not take for a new lease: one the host already holds on
// any interface, the LAN's network or broadcast address, a default gateway, or an address not
// inside any prefix of the LAN interface. An address a lease already holds passes.
func (l *LAN) Check(a netip.Addr) error { return l.check(a, true, true) }

func (l *LAN) check(a netip.Addr, gateways, lanPrefix bool) error {
	have, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	var held []hostAddress
	for _, x := range have {
		p, ok := netip.AddrFromSlice(x.IP)
		if !ok {
			continue
		}
		ones, _ := x.Mask.Size()
		h := hostAddress{Prefix: netip.PrefixFrom(p.Unmap(), ones), Ours: x.Label == l.label && ones == 32}
		if link, err := netlink.LinkByIndex(x.LinkIndex); err == nil {
			h.OnLAN = link.Attrs().Name == l.name
		}
		held = append(held, h)
	}
	var gw []netip.Addr
	if gateways {
		routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
		if err != nil {
			return err
		}
		for _, r := range routes {
			if r.Dst == nil || r.Dst.IP.IsUnspecified() {
				if g, ok := netip.AddrFromSlice(r.Gw); ok {
					gw = append(gw, g.Unmap())
				}
			}
		}
	}
	if err := refuseHostAddress(a, held, gw); err != nil {
		return err
	}
	if lanPrefix {
		if err := checkLANPrefix(a, l.name, held); err != nil {
			return err
		}
	}
	return nil
}

// checkLANPrefix refuses a when it is not inside any non-/32 prefix of the LAN interface. A
// layer-2 announcement cannot reach beyond the interface's own subnet.
func checkLANPrefix(a netip.Addr, lanName string, held []hostAddress) error {
	for _, h := range held {
		if h.OnLAN && !h.Ours && h.Prefix.Bits() < 32 && h.Prefix.Contains(a) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not inside any prefix of %s", ErrOutsideLAN, a, lanName)
}

// hostAddress is an address of the host: its prefix, whether it is on the LAN interface, and whether
// a lease put it there.
type hostAddress struct {
	Prefix netip.Prefix
	OnLAN  bool
	Ours   bool
}

// refuseHostAddress is Check on what the host holds: an address of the host that no lease put there,
// the network and broadcast addresses of the LAN interface's networks, and the gateways.
func refuseHostAddress(a netip.Addr, held []hostAddress, gateways []netip.Addr) error {
	for _, h := range held {
		switch {
		case h.Ours:
			continue
		case h.Prefix.Addr() == a:
			return fmt.Errorf("%w: %s is an address of the host", ErrHostAddress, a)
		case h.OnLAN && h.Prefix.Bits() < 31 && h.Prefix.Contains(a) && (a == h.Prefix.Masked().Addr() || a == lastAddr(h.Prefix)):
			return fmt.Errorf("%w: %s is the network or broadcast address of %s", ErrHostAddress, a, h.Prefix.Masked())
		}
	}
	if slices.Contains(gateways, a) {
		return fmt.Errorf("%w: %s is a default gateway", ErrHostAddress, a)
	}
	return nil
}

// lastAddr is the broadcast address of the prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	for i := p.Bits(); i < 32; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	return netip.AddrFrom4(b)
}

// Remove takes the address off the interface.
func (l *LAN) Remove(a netip.Addr) error {
	link, err := l.link()
	if err != nil {
		return err
	}
	if err := netlink.AddrDel(link, hostAddr(a)); err != nil && err != syscall.EADDRNOTAVAIL {
		return err
	}
	return nil
}

// Held lists the /32 addresses of the interface that carry our label.
func (l *LAN) Held() ([]netip.Addr, error) {
	link, err := l.link()
	if err != nil {
		return nil, err
	}
	have, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, x := range have {
		if ones, _ := x.Mask.Size(); x.Label == l.label && ones == 32 {
			if a, ok := netip.AddrFromSlice(x.IP); ok {
				out = append(out, a.Unmap())
			}
		}
	}
	return out, nil
}

// Probe sends one ARP probe for a and returns ErrTaken when any host answers within 500 ms.
// Use it before taking a new lease to detect another LAN host that holds the address (RFC 5227
// §2.1). The probe uses 0.0.0.0 as the sender IP, so the host does not claim the address;
// any ARP packet with a as the sender protocol address means another host holds it.
// Renewals do not probe: the host already holds the address.
func (l *LAN) Probe(a netip.Addr) error {
	link, err := l.link()
	if err != nil {
		return err
	}
	hw := link.Attrs().HardwareAddr
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(etherTypeARP)))
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	to := &syscall.SockaddrLinklayer{Protocol: htons(etherTypeARP), Ifindex: link.Attrs().Index, Halen: 6}
	copy(to.Addr[:], broadcast)
	frame, err := ARPProbe(hw, a)
	if err != nil {
		return err
	}
	if err := syscall.Sendto(fd, frame, 0, to); err != nil {
		return err
	}
	tv := syscall.Timeval{Sec: 0, Usec: 500000}
	if err := syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv); err != nil {
		return err
	}
	target := a.Unmap().As4()
	buf := make([]byte, 42)
	for {
		n, _, err := syscall.Recvfrom(fd, buf, 0)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
				return nil
			}
			return err
		}
		if n < 42 || buf[12] != 0x08 || buf[13] != 0x06 {
			continue
		}
		var senderIP [4]byte
		copy(senderIP[:], buf[28:32])
		if senderIP == target {
			return ErrTaken
		}
	}
}

// Announce sends the gratuitous ARP of the address on the interface.
func (l *LAN) Announce(a netip.Addr) error {
	link, err := l.link()
	if err != nil {
		return err
	}
	hw := link.Attrs().HardwareAddr
	frame, err := ARPAnnouncement(hw, a)
	if err != nil {
		return err
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_RAW, int(htons(etherTypeARP)))
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	to := &syscall.SockaddrLinklayer{Protocol: htons(etherTypeARP), Ifindex: link.Attrs().Index, Halen: 6}
	copy(to.Addr[:], broadcast)
	return syscall.Sendto(fd, frame, 0, to)
}

const etherTypeARP = 0x0806

var broadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// ARPProbe returns the Ethernet frame of an ARP probe from hw for a: an ARP request whose sender
// IP is 0.0.0.0 (RFC 5227 §2.1), so the host does not claim the address. Any host that holds a
// answers with its MAC and address.
func ARPProbe(hw net.HardwareAddr, a netip.Addr) ([]byte, error) {
	a = a.Unmap()
	if len(hw) != 6 || !a.Is4() {
		return nil, fmt.Errorf("an ARP probe needs a 6-byte hardware address and an IPv4 address")
	}
	f := make([]byte, 0, 42)
	f = append(f, broadcast...)
	f = append(f, hw...)
	f = append(f, 0x08, 0x06)
	f = append(f, 0, 1, 0x08, 0, 6, 4)
	f = append(f, 0, 1)             // request
	f = append(f, hw...)            // sender hardware
	f = append(f, 0, 0, 0, 0)       // sender IP: 0.0.0.0
	f = append(f, 0, 0, 0, 0, 0, 0) // target hardware: zeros
	f = append(f, a.AsSlice()...)   // target IP: the address being probed
	return f, nil
}

// ARPAnnouncement returns the Ethernet frame of an unsolicited ARP reply from hw announcing a: a
// broadcast whose sender and target protocol addresses are both a.
func ARPAnnouncement(hw net.HardwareAddr, a netip.Addr) ([]byte, error) {
	a = a.Unmap()
	if len(hw) != 6 || !a.Is4() {
		return nil, fmt.Errorf("an ARP announcement needs a 6-byte hardware address and an IPv4 address")
	}
	f := make([]byte, 0, 42)
	f = append(f, broadcast...)
	f = append(f, hw...)
	f = append(f, 0x08, 0x06)          // EtherType ARP
	f = append(f, 0, 1, 0x08, 0, 6, 4) // Ethernet, IPv4, address lengths
	f = append(f, 0, 2)                // reply
	f = append(f, hw...)               // sender hardware
	f = append(f, a.AsSlice()...)      // sender protocol
	f = append(f, broadcast...)        // target hardware
	f = append(f, a.AsSlice()...)      // target protocol
	return f, nil
}
