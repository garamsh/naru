package dhcpserver_test

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/dhcp4"
	"github.com/garamsh/naru/tooling/qemu/internal/dhcpserver"
)

var (
	serverMAC  = net.HardwareAddr{0x02, 0, 0, 0, 0xfe, 0xfe}
	hostMAC    = net.HardwareAddr{0x02, 0, 0, 0, 0x04, 0x01}
	otherMAC   = net.HardwareAddr{0x02, 0, 0, 0, 0x05, 0x01}
	strangeMAC = net.HardwareAddr{0x02, 0, 0, 0, 0x09, 0x09}
	serverIP   = netip.MustParseAddr("10.78.0.254")
	hostIP     = netip.MustParseAddr("10.78.0.4")
	otherIP    = netip.MustParseAddr("10.78.0.5")
	dnsIP      = netip.MustParseAddr("10.78.0.253")
	ntpIP      = netip.MustParseAddr("10.78.0.252")
)

func newServer(t *testing.T) *dhcpserver.Server {
	t.Helper()
	s, err := dhcpserver.New(dhcpserver.Config{
		Prefix: netip.MustParsePrefix("10.78.0.0/24"), Address: serverIP, MAC: serverMAC,
		Router: serverIP, DNS: []netip.Addr{dnsIP}, NTP: []netip.Addr{ntpIP}, Lease: time.Hour,
		Reservations: []dhcpserver.Reservation{{MAC: hostMAC, Address: hostIP}, {MAC: otherMAC, Address: otherIP}},
	})
	require.NoError(t, err)
	return s
}

// segment is a transport for a real DHCP client that frames the client's messages the way a NIC puts
// them on the wire, hands them to the server, and gives the client what the server answers, parsed
// back from the frame by code of this test's own.
type segment struct {
	t      *testing.T
	server *dhcpserver.Server
	mac    net.HardwareAddr
	mu     sync.Mutex
	replys chan []byte
}

func newSegment(t *testing.T, s *dhcpserver.Server, mac net.HardwareAddr) *segment {
	return &segment{t: t, server: s, mac: mac, replys: make(chan []byte, 8)}
}

func (g *segment) Send(from, to netip.Addr, msg []byte) error {
	dstMAC, dstIP := net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, netip.AddrFrom4([4]byte{255, 255, 255, 255})
	if to.IsValid() && !to.IsUnspecified() {
		dstMAC, dstIP = serverMAC, to
	}
	if !from.IsValid() {
		from = netip.AddrFrom4([4]byte{})
	}
	if out := g.server.Respond(clientFrame(g.mac, dstMAC, from, dstIP, msg)); out != nil {
		parsed := udpTo68(g.t, out, g.mac)
		g.replys <- parsed
	}
	return nil
}

func (g *segment) Recv(ctx context.Context) ([]byte, error) {
	select {
	case b := <-g.replys:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// clock fires every timer at once: the test has no use for waiting.
type clock struct{}

func (clock) Now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
func (clock) After(time.Duration) <-chan time.Time {
	c := make(chan time.Time)
	return c
}

type nopHandler struct{}

func (nopHandler) Bound(dhcp4.Lease)   {}
func (nopHandler) Expired(dhcp4.Lease) {}

func TestServer_ARealClientIsGivenItsReservationWithTheFleetsOptions(t *testing.T) {
	s := newServer(t)
	c := &dhcp4.Client{
		HWAddr: hostMAC, Transport: newSegment(t, s, hostMAC), Clock: clock{}, Handler: nopHandler{},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	l, err := c.Acquire(ctx)

	require.NoError(t, err)
	assert.Equal(t, netip.MustParsePrefix("10.78.0.4/24"), l.Address, "the reserved address with the network's mask")
	assert.Equal(t, serverIP, l.Router)
	assert.Equal(t, []netip.Addr{dnsIP}, l.DNS)
	assert.Equal(t, []netip.Addr{ntpIP}, l.NTP, "option 42, which init sets the clock from when spec.time names none")
	assert.Equal(t, serverIP, l.Server)
	assert.Equal(t, time.Hour, l.Duration)
	assert.Equal(t, 30*time.Minute, l.T1)
	assert.Equal(t, 52*time.Minute+30*time.Second, l.T2)
}

func TestServer_EachMACIsGivenItsOwnAddress(t *testing.T) {
	s := newServer(t)
	for mac, want := range map[string]netip.Addr{hostMAC.String(): hostIP, otherMAC.String(): otherIP} {
		hw, err := net.ParseMAC(mac)
		require.NoError(t, err)
		c := &dhcp4.Client{HWAddr: hw, Transport: newSegment(t, s, hw), Clock: clock{}, Handler: nopHandler{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

		l, err := c.Acquire(t.Context())

		require.NoError(t, err)
		assert.Equal(t, want, l.Address.Addr(), mac)
	}
}

func TestServer_AnUnreservedMACIsNotAnswered(t *testing.T) {
	s := newServer(t)

	for _, typ := range []dhcp4.MessageType{dhcp4.Discover, dhcp4.Request} {
		assert.Nil(t, s.Respond(clientFrame(strangeMAC, broadcast, netip.IPv4Unspecified(), bcastIP, message(strangeMAC, typ, nil))), typ)
	}
}

func TestServer_ARequestForAnotherAddressIsRefused(t *testing.T) {
	s := newServer(t)
	req := message(hostMAC, dhcp4.Request, map[byte][]byte{dhcp4.OptRequestedIP: otherIP.AsSlice()})

	out := s.Respond(clientFrame(hostMAC, broadcast, netip.IPv4Unspecified(), bcastIP, req))

	require.NotNil(t, out)
	f := parse(t, out)
	assert.Equal(t, dhcp4.Nak, f.msg.Type())
	assert.False(t, f.msg.YIAddr.IsValid() && !f.msg.YIAddr.IsUnspecified(), "a NAK gives no address")
	assert.Equal(t, bcastIP, f.dstIP)
	assert.Equal(t, hostMAC, f.dstMAC, "to the client that asked")
}

func TestServer_ARequestNamingAnotherServerIsNotAnswered(t *testing.T) {
	s := newServer(t)
	other := netip.MustParseAddr("10.78.0.1")
	req := message(hostMAC, dhcp4.Request, map[byte][]byte{dhcp4.OptRequestedIP: hostIP.AsSlice(), dhcp4.OptServerID: other.AsSlice()})

	assert.Nil(t, s.Respond(clientFrame(hostMAC, broadcast, netip.IPv4Unspecified(), bcastIP, req)))
}

func TestServer_ARenewalIsAnsweredToTheClientsAddress(t *testing.T) {
	s := newServer(t)
	req := dhcp4.Message{XID: 7, CIAddr: hostIP, CHAddr: hostMAC, Options: map[byte][]byte{dhcp4.OptMessageType: {byte(dhcp4.Request)}}}

	out := s.Respond(clientFrame(hostMAC, serverMAC, hostIP, serverIP, req.Encode()))

	require.NotNil(t, out)
	f := parse(t, out)
	assert.Equal(t, dhcp4.Ack, f.msg.Type())
	assert.Equal(t, hostIP, f.msg.YIAddr)
	assert.Equal(t, hostIP, f.dstIP)
	assert.Equal(t, serverIP, f.srcIP)
	assert.Equal(t, serverMAC, f.srcMAC)
}

func TestServer_ARPForTheServersAddressIsAnswered(t *testing.T) {
	s := newServer(t)

	out := s.Respond(arpRequest(hostMAC, hostIP, serverIP))

	require.NotNil(t, out)
	assert.Equal(t, uint16(0x0806), binary.BigEndian.Uint16(out[12:]))
	assert.Equal(t, hostMAC, net.HardwareAddr(out[0:6]), "to the asker")
	assert.Equal(t, serverMAC, net.HardwareAddr(out[6:12]))
	arp := out[14:]
	assert.Equal(t, uint16(2), binary.BigEndian.Uint16(arp[6:]), "a reply")
	assert.Equal(t, serverMAC, net.HardwareAddr(arp[8:14]), "the server's MAC is the answer")
	assert.Equal(t, serverIP.AsSlice(), arp[14:18])
	assert.Equal(t, hostMAC, net.HardwareAddr(arp[18:24]))
	assert.Equal(t, hostIP.AsSlice(), arp[24:28])
	assert.GreaterOrEqual(t, len(out), 60, "an Ethernet frame is at least 60 bytes")
}

func TestServer_ARPForAnotherAddressIsNotAnswered(t *testing.T) {
	s := newServer(t)

	assert.Nil(t, s.Respond(arpRequest(hostMAC, hostIP, netip.MustParseAddr("10.78.0.77"))))
}

func TestServer_ItDoesNotAnswerItsOwnFramesOrFramesForAnotherMAC(t *testing.T) {
	s := newServer(t)
	discover := message(hostMAC, dhcp4.Discover, nil)

	assert.Nil(t, s.Respond(clientFrame(serverMAC, broadcast, serverIP, bcastIP, discover)), "its own frame comes back on the multicast group")
	assert.Nil(t, s.Respond(clientFrame(hostMAC, otherMAC, hostIP, otherIP, discover)), "a frame for a neighbour")
}

func TestServer_AMalformedFrameIsNotAnswered(t *testing.T) {
	s := newServer(t)
	good := clientFrame(hostMAC, broadcast, netip.IPv4Unspecified(), bcastIP, message(hostMAC, dhcp4.Discover, nil))

	for name, b := range map[string][]byte{
		"empty": nil, "short": good[:20], "truncated": good[:60], "not DHCP": append(append([]byte{}, good[:14]...), make([]byte, 60)...),
	} {
		assert.Nil(t, s.Respond(b), name)
	}
}

func TestServe_AnswersFramesOfTheSegmentUntilTheContextEnds(t *testing.T) {
	s := newServer(t)
	conn := newPipe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, conn) }()

	conn.in <- clientFrame(hostMAC, broadcast, netip.IPv4Unspecified(), bcastIP, message(hostMAC, dhcp4.Discover, nil))

	select {
	case out := <-conn.out:
		assert.Equal(t, dhcp4.Offer, parse(t, out).msg.Type())
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return when its context ended")
	}
}

func TestNew_RefusesWhatCannotWork(t *testing.T) {
	good := dhcpserver.Config{
		Prefix: netip.MustParsePrefix("10.78.0.0/24"), Address: serverIP, MAC: serverMAC, Lease: time.Hour,
		Reservations: []dhcpserver.Reservation{{MAC: hostMAC, Address: hostIP}},
	}
	for name, mod := range map[string]func(*dhcpserver.Config){
		"a server outside the prefix":      func(c *dhcpserver.Config) { c.Address = netip.MustParseAddr("192.168.0.1") },
		"a reservation outside the prefix": func(c *dhcpserver.Config) { c.Reservations[0].Address = netip.MustParseAddr("192.168.0.9") },
		"the server's own address reserved": func(c *dhcpserver.Config) {
			c.Reservations = append(c.Reservations, dhcpserver.Reservation{MAC: otherMAC, Address: serverIP})
		},
		"one address for two MACs": func(c *dhcpserver.Config) {
			c.Reservations = append(c.Reservations, dhcpserver.Reservation{MAC: otherMAC, Address: hostIP})
		},
		"two addresses for one MAC": func(c *dhcpserver.Config) {
			c.Reservations = append(c.Reservations, dhcpserver.Reservation{MAC: hostMAC, Address: otherIP})
		},
		"a lease of no time": func(c *dhcpserver.Config) { c.Lease = 0 },
		"no server MAC":      func(c *dhcpserver.Config) { c.MAC = nil },
	} {
		c := good
		c.Reservations = append([]dhcpserver.Reservation{}, good.Reservations...)
		mod(&c)
		_, err := dhcpserver.New(c)
		assert.Error(t, err, name)
	}
	_, err := dhcpserver.New(good)
	assert.NoError(t, err, "the control: the configuration without the faults")
}
