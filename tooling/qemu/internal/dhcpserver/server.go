// Package dhcpserver is the DHCPv4 server of the QEMU tests' fleet, run in the test process. The
// fleet's network is the multicast segment QEMU joins its Nodes' NICs to, and it has no DHCP server of
// its own; a host that boots from an installation seed asks for its address with DHCP (ADR 0058), so
// the test gives each such host's MAC a reservation, as an operator reserves an address on the DHCP
// server of the real network. A MAC without a reservation is not answered.
//
// The message code is lib/init/dhcp4's, the same the host's client speaks. Handle is the decision on
// one message; Server.Respond turns an Ethernet frame into the frame to answer it with; Serve moves
// frames between a Frames and Respond.
package dhcpserver

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/garamsh/naru/lib/init/dhcp4"
)

// Reservation is the address the server gives the host whose NIC has the MAC.
type Reservation struct {
	MAC     net.HardwareAddr
	Address netip.Addr
}

// Config is what the server answers with.
type Config struct {
	// Prefix is the network the reservations are in; its length is the subnet mask the server gives.
	Prefix netip.Prefix
	// Address and MAC are the server's own, which it identifies itself and answers ARP for.
	Address netip.Addr
	MAC     net.HardwareAddr
	// Router, DNS and NTP are the options the server gives with every address.
	Router netip.Addr
	DNS    []netip.Addr
	NTP    []netip.Addr
	// Lease is how long an address is leased for; the client renews at half of it.
	Lease        time.Duration
	Reservations []Reservation
}

// Server answers DHCP requests of the hosts it holds a reservation for.
type Server struct {
	cfg      Config
	reserved map[string]netip.Addr
}

// New returns the server for cfg, or the first thing wrong with it.
func New(cfg Config) (*Server, error) {
	if !cfg.Prefix.IsValid() || !cfg.Prefix.Addr().Is4() {
		return nil, errors.New("dhcpserver: the prefix is not an IPv4 prefix")
	}
	cfg.Prefix = cfg.Prefix.Masked()
	if !cfg.Address.Is4() || !cfg.Prefix.Contains(cfg.Address) {
		return nil, fmt.Errorf("dhcpserver: the server address %s is not in %s", cfg.Address, cfg.Prefix)
	}
	if len(cfg.MAC) != 6 {
		return nil, errors.New("dhcpserver: the server's MAC is not an Ethernet address")
	}
	if cfg.Lease < 8*time.Second {
		return nil, errors.New("dhcpserver: the lease is shorter than 8 seconds")
	}
	for _, a := range append(append([]netip.Addr{cfg.Router}, cfg.DNS...), cfg.NTP...) {
		if a.IsValid() && !a.Is4() {
			return nil, fmt.Errorf("dhcpserver: %s is not an IPv4 address", a)
		}
	}
	s := &Server{cfg: cfg, reserved: map[string]netip.Addr{}}
	taken := map[netip.Addr]bool{cfg.Address: true}
	for _, r := range cfg.Reservations {
		if len(r.MAC) != 6 {
			return nil, fmt.Errorf("dhcpserver: the reservation of %s has no Ethernet MAC", r.Address)
		}
		if !r.Address.Is4() || !cfg.Prefix.Contains(r.Address) {
			return nil, fmt.Errorf("dhcpserver: the reservation %s for %s is not in %s", r.Address, r.MAC, cfg.Prefix)
		}
		if taken[r.Address] {
			return nil, fmt.Errorf("dhcpserver: %s is reserved twice or is the server's own", r.Address)
		}
		if _, ok := s.reserved[r.MAC.String()]; ok {
			return nil, fmt.Errorf("dhcpserver: %s has two reservations", r.MAC)
		}
		taken[r.Address] = true
		s.reserved[r.MAC.String()] = r.Address
	}
	return s, nil
}

// Handle returns the answer to a client's message and true, or false when the server does not answer
// it: a message of a MAC without a reservation, a message for another server, and the types the
// harness has no use for. A DISCOVER is answered with an OFFER of the reserved address. A REQUEST for
// that address, whether it selects the server, renews or rebinds, is answered with an ACK, and a
// REQUEST for any other with a NAK.
func (s *Server) Handle(m dhcp4.Message) (dhcp4.Message, bool) {
	if m.Reply {
		return dhcp4.Message{}, false
	}
	addr, ok := s.reserved[m.CHAddr.String()]
	if !ok {
		return dhcp4.Message{}, false
	}
	switch m.Type() {
	case dhcp4.Discover:
		return s.answer(m, dhcp4.Offer, addr), true
	case dhcp4.Request:
		if id := m.Options[dhcp4.OptServerID]; len(id) == 4 && netip.AddrFrom4([4]byte(id)) != s.cfg.Address {
			return dhcp4.Message{}, false
		}
		want := m.CIAddr
		if v := m.Options[dhcp4.OptRequestedIP]; len(v) == 4 {
			want = netip.AddrFrom4([4]byte(v))
		}
		if want != addr {
			nak := s.reply(m, dhcp4.Nak)
			return nak, true
		}
		return s.answer(m, dhcp4.Ack, addr), true
	}
	return dhcp4.Message{}, false
}

// reply is the message to m of type t, with no address and no option but the server's identifier.
func (s *Server) reply(m dhcp4.Message, t dhcp4.MessageType) dhcp4.Message {
	id := s.cfg.Address.As4()
	return dhcp4.Message{
		Reply: true, XID: m.XID, Broadcast: m.Broadcast, CHAddr: m.CHAddr,
		Options: map[byte][]byte{dhcp4.OptMessageType: {byte(t)}, dhcp4.OptServerID: id[:]},
	}
}

// answer is the OFFER or ACK that gives addr with the server's options.
func (s *Server) answer(m dhcp4.Message, t dhcp4.MessageType, addr netip.Addr) dhcp4.Message {
	out := s.reply(m, t)
	out.YIAddr = addr
	secs := func(d time.Duration) []byte { return binary.BigEndian.AppendUint32(nil, uint32(d/time.Second)) }
	out.Options[dhcp4.OptLeaseTime] = secs(s.cfg.Lease)
	out.Options[dhcp4.OptRenewalTime] = secs(s.cfg.Lease / 2)
	out.Options[dhcp4.OptRebindingTime] = secs(s.cfg.Lease / 8 * 7)
	out.Options[dhcp4.OptSubnetMask] = net.CIDRMask(s.cfg.Prefix.Bits(), 32)
	list := func(code byte, as ...netip.Addr) {
		var v []byte
		for _, a := range as {
			if a.IsValid() {
				b := a.As4()
				v = append(v, b[:]...)
			}
		}
		if len(v) > 0 {
			out.Options[code] = v
		}
	}
	list(dhcp4.OptRouter, s.cfg.Router)
	list(dhcp4.OptDNS, s.cfg.DNS...)
	list(dhcp4.OptNTP, s.cfg.NTP...)
	return out
}

// Respond returns the Ethernet frame that answers frame, or nil when there is none: an ARP request for
// the server's address is answered with an ARP reply, a DHCP message that Handle answers with the
// frame of its answer, and anything else, the server's own frames included, not at all.
func (s *Server) Respond(frame []byte) []byte {
	if len(frame) < etherLen || bytes.Equal(frame[6:12], s.cfg.MAC) {
		return nil
	}
	if dst := frame[:6]; !bytes.Equal(dst, s.cfg.MAC) && !bytes.Equal(dst, broadcastMAC) {
		return nil
	}
	switch binary.BigEndian.Uint16(frame[12:]) {
	case etherTypeARP:
		return s.arpReply(frame)
	case etherTypeIP:
		payload := udpPayloadTo(frame, dhcp4.ServerPort)
		if payload == nil {
			return nil
		}
		m, err := dhcp4.Decode(payload)
		if err != nil {
			return nil
		}
		out, ok := s.Handle(m)
		if !ok {
			return nil
		}
		return s.dhcpFrame(m, out)
	}
	return nil
}
