package dhcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"golang.org/x/net/ipv4"
)

// Frames is the Ethernet segment the server is on: Read returns the next frame on it, Write puts one
// on it, and Close ends both, which makes a pending Read return an error.
type Frames interface {
	Read() ([]byte, error)
	Write(frame []byte) error
	Close() error
}

// Serve answers the frames of conn until ctx ends or conn fails, and closes conn. It returns nil when
// ctx ended.
func (s *Server) Serve(ctx context.Context, conn Frames) error {
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	for {
		frame, err := conn.Read()
		if err != nil {
			_ = conn.Close()
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("dhcpserver: read: %w", err)
		}
		if out := s.Respond(frame); out != nil {
			if err := conn.Write(out); err != nil && ctx.Err() == nil {
				return fmt.Errorf("dhcpserver: write: %w", err)
			}
		}
	}
}

// multicast is QEMU's socket network: every Ethernet frame of a NIC is one UDP datagram to the group
// address, and every NIC of the group reads them all, this process's own included.
type multicast struct {
	conn  *net.UDPConn
	group *net.UDPAddr
}

// ListenMulticast joins the multicast group, "230.78.0.1:port", that Nodes' NICs are on
// (-netdev socket,mcast=<group>,localaddr=127.0.0.1) and returns the segment.
func ListenMulticast(group string) (Frames, error) {
	addr, err := net.ResolveUDPAddr("udp4", group)
	if err != nil {
		return nil, fmt.Errorf("dhcpserver: %w", err)
	}
	lo, err := loopback()
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenMulticastUDP("udp4", lo, addr)
	if err != nil {
		return nil, fmt.Errorf("dhcpserver: join %s: %w", group, err)
	}
	p := ipv4.NewPacketConn(conn)
	if err := p.SetMulticastInterface(lo); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dhcpserver: %w", err)
	}
	if err := p.SetMulticastLoopback(true); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dhcpserver: %w", err)
	}
	return &multicast{conn: conn, group: addr}, nil
}

// loopback is the interface that has 127.0.0.1, the localaddr of the Nodes' NICs.
func loopback() (*net.Interface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("dhcpserver: %w", err)
	}
	for i := range ifs {
		if ifs[i].Flags&net.FlagLoopback != 0 {
			return &ifs[i], nil
		}
	}
	return nil, errors.New("dhcpserver: no loopback interface")
}

func (m *multicast) Read() ([]byte, error) {
	buf := make([]byte, 65536)
	n, _, err := m.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func (m *multicast) Write(frame []byte) error {
	_, err := m.conn.WriteToUDP(frame, m.group)
	return err
}

func (m *multicast) Close() error { return m.conn.Close() }
