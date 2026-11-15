package linux

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"golang.org/x/sys/unix"

	"github.com/garamsh/naru/lib/init/dhcp4"
)

// packetTransport is dhcp4.Transport over the kernel's sockets. A packet socket bound to the link
// carries every message the client sends before it has an address and receives every reply, which
// is why no libc resolver or address is needed; a UDP socket bound to the leased address carries
// the unicast renewal, so that the kernel finds the server's MAC. It is a thin adapter: the
// framing is buildFrame and parseFrame, and the exchange is dhcp4's.
type packetTransport struct {
	link    link
	raw     int
	udp     int
	udpFrom netip.Addr
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// openPacketTransport opens the sockets of l.
func openPacketTransport(l link) (dhcp4.Transport, error) {
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_IP)))
	if err != nil {
		return nil, fmt.Errorf("packet socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: l.Index}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("bind the packet socket to %s: %w", l.Name, err)
	}
	// Recv wakes up to look at its context.
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 200_000}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("set the receive timeout: %w", err)
	}
	return &packetTransport{link: l, raw: fd, udp: -1}, nil
}

func (t *packetTransport) Send(from, to netip.Addr, msg []byte) error {
	if from.IsValid() && to.IsValid() {
		return t.sendUnicast(from, to, msg)
	}
	sa := &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: t.link.Index, Halen: 6}
	copy(sa.Addr[:], broadcastMAC)
	if err := unix.Sendto(t.raw, buildFrame(t.link.MAC, from, to, msg), 0, sa); err != nil {
		return fmt.Errorf("send on %s: %w", t.link.Name, err)
	}
	return nil
}

func (t *packetTransport) sendUnicast(from, to netip.Addr, msg []byte) error {
	if t.udp >= 0 && t.udpFrom != from {
		_ = unix.Close(t.udp)
		t.udp = -1
	}
	if t.udp < 0 {
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("udp socket: %w", err)
		}
		bind := func() error {
			if err := unix.BindToDevice(fd, t.link.Name); err != nil {
				return fmt.Errorf("bind to %s: %w", t.link.Name, err)
			}
			if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
				return err
			}
			return unix.Bind(fd, &unix.SockaddrInet4{Port: dhcp4.ClientPort, Addr: from.As4()})
		}
		if err := bind(); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("udp socket for %s: %w", from, err)
		}
		t.udp, t.udpFrom = fd, from
	}
	if err := unix.Sendto(t.udp, msg, 0, &unix.SockaddrInet4{Port: dhcp4.ServerPort, Addr: to.As4()}); err != nil {
		return fmt.Errorf("send to %s: %w", to, err)
	}
	return nil
}

func (t *packetTransport) Recv(ctx context.Context) ([]byte, error) {
	buf := make([]byte, 2048)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, _, err := unix.Recvfrom(t.raw, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return nil, fmt.Errorf("receive on %s: %w", t.link.Name, err)
		}
		if payload, err := parseFrame(buf[:n]); err == nil {
			return payload, nil
		}
	}
}
