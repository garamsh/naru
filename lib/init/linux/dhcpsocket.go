package linux

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"golang.org/x/net/bpf"
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

// dhcpFilter is the classic BPF program on the packet socket: it admits an unfragmented IPv4
// packet that carries UDP to the client's port, so that the kernel copies no other frame of the
// link to user space. The program reads the header length out of the packet, because IPv4 options
// move the UDP header.
func dhcpFilter() ([]bpf.RawInstruction, error) {
	const (
		ethertype  = 12
		ipFragment = etherLen + 6
		ipProto    = etherLen + 9
		ipHeader   = etherLen
		udpDstPort = 2
	)
	return bpf.Assemble([]bpf.Instruction{
		bpf.LoadAbsolute{Off: ethertype, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: etherTypeIP, SkipTrue: 8},
		bpf.LoadAbsolute{Off: ipProto, Size: 1},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: protoUDP, SkipTrue: 6},
		bpf.LoadAbsolute{Off: ipFragment, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpBitsSet, Val: 0x3fff, SkipTrue: 4},
		bpf.LoadMemShift{Off: ipHeader},
		bpf.LoadIndirect{Off: ipHeader + udpDstPort, Size: 2},
		bpf.JumpIf{Cond: bpf.JumpNotEqual, Val: dhcp4.ClientPort, SkipTrue: 1},
		bpf.RetConstant{Val: 0xffff},
		bpf.RetConstant{Val: 0},
	})
}

// openPacketSocket returns a packet socket on l that the kernel feeds only the frames of
// dhcpFilter. The filter is attached before the socket is bound, so that no other frame is queued
// in between.
func openPacketSocket(l link) (int, error) {
	prog, err := dhcpFilter()
	if err != nil {
		return -1, fmt.Errorf("assemble the packet filter: %w", err)
	}
	filter := make([]unix.SockFilter, len(prog))
	for i, in := range prog {
		filter[i] = unix.SockFilter{Code: in.Op, Jt: in.Jt, Jf: in.Jf, K: in.K}
	}
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("packet socket: %w", err)
	}
	fail := func(err error) (int, error) {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := unix.SetsockoptSockFprog(fd, unix.SOL_SOCKET, unix.SO_ATTACH_FILTER, &unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}); err != nil {
		return fail(fmt.Errorf("attach the packet filter: %w", err))
	}
	if err := unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_IP), Ifindex: l.Index}); err != nil {
		return fail(fmt.Errorf("bind the packet socket to %s: %w", l.Name, err))
	}
	// Recv wakes up to look at its context.
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &unix.Timeval{Usec: 200_000}); err != nil {
		return fail(fmt.Errorf("set the receive timeout: %w", err))
	}
	return fd, nil
}

// openPacketTransport opens the sockets of l. The client closes the transport at the end of each
// exchange and opens another for the next.
func openPacketTransport(l link) (dhcp4.Transport, error) {
	fd, err := openPacketSocket(l)
	if err != nil {
		return nil, err
	}
	return &packetTransport{link: l, raw: fd, udp: -1}, nil
}

// Close closes both sockets, and the frames queued on them are dropped.
func (t *packetTransport) Close() error {
	err := unix.Close(t.raw)
	if t.udp >= 0 {
		err = errors.Join(err, unix.Close(t.udp))
		t.udp = -1
	}
	return err
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
