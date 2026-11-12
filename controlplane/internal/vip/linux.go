package vip

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Linux is the Host of a Node whose card is Link: it adds the address with netlink and announces it
// with gratuitous ARP on an AF_PACKET socket.
type Linux struct {
	Link    string
	Address netip.Prefix
}

func (l Linux) link() (netlink.Link, error) { return netlink.LinkByName(l.Link) }

func (l Linux) addr() *netlink.Addr {
	return &netlink.Addr{IPNet: &net.IPNet{IP: l.Address.Addr().AsSlice(), Mask: net.CIDRMask(l.Address.Bits(), 32)}}
}

// Holds reports whether the card has the address.
func (l Linux) Holds() (bool, error) {
	link, err := l.link()
	if err != nil {
		return false, err
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return false, err
	}
	for _, a := range addrs {
		if a.IP.Equal(l.Address.Addr().AsSlice()) {
			return true, nil
		}
	}
	return false, nil
}

// Acquire adds the address to the card and announces it.
func (l Linux) Acquire() error {
	link, err := l.link()
	if err != nil {
		return err
	}
	if err := netlink.AddrReplace(link, l.addr()); err != nil {
		return fmt.Errorf("add %s to %s: %w", l.Address, l.Link, err)
	}
	return l.Announce()
}

// Release removes the address from the card.
func (l Linux) Release() error {
	link, err := l.link()
	if err != nil {
		return err
	}
	return netlink.AddrDel(link, l.addr())
}

// Announce sends the address's gratuitous ARP twice, once as a request and once as a reply, to every
// host on the card's network, so that their caches name this Node's card.
func (l Linux) Announce() error {
	ifi, err := net.InterfaceByName(l.Link)
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW, int(htons(unix.ETH_P_ARP)))
	if err != nil {
		return fmt.Errorf("open a packet socket: %w", err)
	}
	defer func() { _ = unix.Close(fd) }()
	dst := unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ARP), Ifindex: ifi.Index, Halen: 6}
	copy(dst.Addr[:], broadcast)
	for _, op := range []uint16{1, 2} {
		if err := unix.Sendto(fd, ARPFrame(ifi.HardwareAddr, l.Address.Addr(), op), 0, &dst); err != nil {
			return fmt.Errorf("send the ARP announcement: %w", err)
		}
	}
	return nil
}

var broadcast = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// ARPFrame is the Ethernet frame that announces ip at mac: sender and target are both ip, and op is
// 1 for a request and 2 for a reply. It goes to the broadcast address.
func ARPFrame(mac net.HardwareAddr, ip netip.Addr, op uint16) []byte {
	b := make([]byte, 14+28)
	copy(b[0:6], broadcast)
	copy(b[6:12], mac)
	binary.BigEndian.PutUint16(b[12:], unix.ETH_P_ARP)
	arp := b[14:]
	binary.BigEndian.PutUint16(arp[0:], 1)      // Ethernet
	binary.BigEndian.PutUint16(arp[2:], 0x0800) // IPv4
	arp[4], arp[5] = 6, 4
	binary.BigEndian.PutUint16(arp[6:], op)
	copy(arp[8:14], mac)
	copy(arp[14:18], ip.AsSlice())
	if op == 2 {
		copy(arp[18:24], broadcast)
	}
	copy(arp[24:28], ip.AsSlice())
	return b
}

// DialProbe reports whether a TCP connection to addr's port is accepted within timeout.
func DialProbe(port string, timeout time.Duration) Prober {
	return func(ctx context.Context, addr netip.Addr) bool {
		d := net.Dialer{Timeout: timeout}
		c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr.String(), port))
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}
}
