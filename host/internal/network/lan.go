package network

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"

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

// Add gives the interface the address.
func (l *LAN) Add(a netip.Addr) error {
	link, err := l.link()
	if err != nil {
		return err
	}
	x := hostAddr(a)
	x.Label = l.label
	if err := netlink.AddrReplace(link, x); err != nil {
		return err
	}
	return nil
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
