// Package dhcp4 is a DHCPv4 client (RFC 2131): it acquires a lease, renews it at T1, rebinds at T2
// and reports its expiry. The packet I/O is a seam, Transport, so that the state machine runs
// against an in-process server; the sockets are an adapter in lib/init/linux
// (docs/architecture/adr/0058-a-host-boots-from-a-join-form-that-names-an-orch-and-an-enrollment-key-and-init-applies-the-configuration-hostd-brings-back.md).
package dhcp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
)

// MessageType is the value of option 53.
type MessageType byte

// The messages the client sends and receives.
const (
	Discover MessageType = 1
	Offer    MessageType = 2
	Request  MessageType = 3
	Ack      MessageType = 5
	Nak      MessageType = 6
)

// The options the client reads or writes.
const (
	OptSubnetMask    byte   = 1
	OptRouter        byte   = 3
	OptDNS           byte   = 6
	OptNTP           byte   = 42
	OptRequestedIP   byte   = 50
	OptLeaseTime     byte   = 51
	OptMessageType   byte   = 53
	OptOverload      byte   = 52
	OptServerID      byte   = 54
	OptParameterList byte   = 55
	OptRenewalTime   byte   = 58
	OptRebindingTime byte   = 59
	OptClientID      byte   = 61
	optPad           byte   = 0
	optEnd           byte   = 255
	opRequest        byte   = 1
	opReply          byte   = 2
	hardwareEthernet byte   = 1
	snameOffset             = 44
	fileOffset              = 108
	headerLen               = 236
	minMessageLen           = 300
	broadcastFlag           = 0x8000
	magicCookie      uint32 = 0x63825363
	ServerPort              = 67
	ClientPort              = 68
)

// Message is one DHCP message.
type Message struct {
	// Reply is true for a message from a server (BOOTP op 2).
	Reply bool
	XID   uint32
	// Broadcast asks the server to reply to the broadcast address.
	Broadcast      bool
	CIAddr, YIAddr netip.Addr
	CHAddr         net.HardwareAddr
	Options        map[byte][]byte
}

// Type returns the message type of option 53, or 0 when the message has none.
func (m Message) Type() MessageType {
	if v := m.Options[OptMessageType]; len(v) == 1 {
		return MessageType(v[0])
	}
	return 0
}

// Encode returns the message as a BOOTP packet, at least 300 bytes long.
func (m Message) Encode() []byte {
	b := make([]byte, headerLen, minMessageLen)
	b[0] = opRequest
	if m.Reply {
		b[0] = opReply
	}
	b[1], b[2] = hardwareEthernet, 6
	binary.BigEndian.PutUint32(b[4:], m.XID)
	if m.Broadcast {
		binary.BigEndian.PutUint16(b[10:], broadcastFlag)
	}
	if m.CIAddr.Is4() {
		a := m.CIAddr.As4()
		copy(b[12:], a[:])
	}
	if m.YIAddr.Is4() {
		a := m.YIAddr.As4()
		copy(b[16:], a[:])
	}
	copy(b[28:44], m.CHAddr)
	b = binary.BigEndian.AppendUint32(b, magicCookie)
	codes := make([]byte, 0, len(m.Options))
	for c := range m.Options {
		codes = append(codes, c)
	}
	slices.Sort(codes)
	for _, c := range codes {
		// RFC 3396: a value of more than 255 bytes is split into instances of the option.
		v := m.Options[c]
		for first := true; first || len(v) > 0; first = false {
			n := min(len(v), 255)
			b = append(b, c, byte(n))
			b = append(b, v[:n]...)
			v = v[n:]
		}
	}
	b = append(b, optEnd)
	for len(b) < minMessageLen {
		b = append(b, optPad)
	}
	return b
}

// Decode parses a BOOTP packet that carries the DHCP magic cookie.
func Decode(b []byte) (Message, error) {
	if len(b) < headerLen+4 {
		return Message{}, fmt.Errorf("dhcp4: a message of %d bytes is shorter than the %d of a header", len(b), headerLen+4)
	}
	if binary.BigEndian.Uint32(b[headerLen:]) != magicCookie {
		return Message{}, errors.New("dhcp4: the message has no DHCP magic cookie")
	}
	if b[1] != hardwareEthernet || b[2] != 6 {
		return Message{}, fmt.Errorf("dhcp4: hardware type %d of length %d is not Ethernet", b[1], b[2])
	}
	m := Message{
		Reply:     b[0] == opReply,
		XID:       binary.BigEndian.Uint32(b[4:]),
		Broadcast: binary.BigEndian.Uint16(b[10:])&broadcastFlag != 0,
		CIAddr:    netip.AddrFrom4([4]byte(b[12:16])),
		YIAddr:    netip.AddrFrom4([4]byte(b[16:20])),
		CHAddr:    slices.Clone(net.HardwareAddr(b[28:34])),
		Options:   map[byte][]byte{},
	}
	// The options field comes first. Option 52 then names the file and sname fields as more
	// options (RFC 2131 section 4.1), read in that order (RFC 3396 section 4.1).
	if err := readOptions(b[headerLen+4:], m.Options); err != nil {
		return Message{}, err
	}
	if v := m.Options[OptOverload]; len(v) == 1 {
		var more [][]byte
		if v[0]&1 != 0 {
			more = append(more, b[fileOffset:headerLen])
		}
		if v[0]&2 != 0 {
			more = append(more, b[snameOffset:fileOffset])
		}
		for _, area := range more {
			if err := readOptions(area, m.Options); err != nil {
				return Message{}, err
			}
		}
	}
	return m, nil
}

// readOptions adds the options of one area to into. An option that occurs again is concatenated to
// its earlier instances (RFC 3396).
func readOptions(opts []byte, into map[byte][]byte) error {
	for len(opts) > 0 {
		code := opts[0]
		if code == optEnd {
			return nil
		}
		if code == optPad {
			opts = opts[1:]
			continue
		}
		if len(opts) < 2 || len(opts) < 2+int(opts[1]) {
			return fmt.Errorf("dhcp4: option %d is cut short", code)
		}
		n := int(opts[1])
		into[code] = append(into[code], opts[2:2+n]...)
		opts = opts[2+n:]
	}
	return nil
}

// addrs reads an option that is a list of IPv4 addresses.
func addrs(v []byte) []netip.Addr {
	var out []netip.Addr
	for ; len(v) >= 4; v = v[4:] {
		out = append(out, netip.AddrFrom4([4]byte(v[:4])))
	}
	return out
}
