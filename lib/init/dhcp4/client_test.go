package dhcp4_test

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
)

var (
	hw       = net.HardwareAddr{0x52, 0x54, 0, 0x12, 0x34, 0x56}
	serverIP = netip.MustParseAddr("10.0.2.2")
	leaseIP  = netip.MustParseAddr("10.0.2.15")
	epoch    = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

// fakeClock moves when the test says so.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*timer
}

type timer struct {
	at time.Time
	ch chan time.Time
}

func newClock() *fakeClock { return &fakeClock{now: epoch} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	return t.ch
}

// advance moves the clock and fires the timers that are due.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	rest := c.timers[:0]
	for _, t := range c.timers {
		if t.at.After(c.now) {
			rest = append(rest, t)
			continue
		}
		t.ch <- c.now
	}
	c.timers = rest
}

// server answers the client in-process, the way a DHCP server on the wire would.
type server struct {
	mu       sync.Mutex
	got      []sent
	replies  chan []byte
	offer    func(*dhcp4.Message)
	ack      func(*dhcp4.Message)
	dropWhen func(dhcp4.Message, sent) bool
	nak      bool
}

type sent struct {
	from, to netip.Addr
	msg      dhcp4.Message
}

func newServer() *server { return &server{replies: make(chan []byte, 16)} }

func (s *server) Send(from, to netip.Addr, b []byte) error {
	m, err := dhcp4.Decode(b)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := sent{from, to, m}
	s.got = append(s.got, rec)
	if s.dropWhen != nil && s.dropWhen(m, rec) {
		return nil
	}
	reply := dhcp4.Message{Reply: true, XID: m.XID, CHAddr: m.CHAddr, Options: map[byte][]byte{
		dhcp4.OptSubnetMask: {255, 255, 255, 0},
		dhcp4.OptRouter:     serverIP.AsSlice(),
		dhcp4.OptDNS:        append(serverIP.AsSlice(), 1, 1, 1, 1),
		dhcp4.OptNTP:        {10, 0, 2, 3},
		dhcp4.OptServerID:   serverIP.AsSlice(),
		dhcp4.OptLeaseTime:  binary.BigEndian.AppendUint32(nil, 1000),
	}, YIAddr: leaseIP}
	switch m.Type() {
	case dhcp4.Discover:
		reply.Options[dhcp4.OptMessageType] = []byte{byte(dhcp4.Offer)}
		if s.offer != nil {
			s.offer(&reply)
		}
	case dhcp4.Request:
		reply.Options[dhcp4.OptMessageType] = []byte{byte(dhcp4.Ack)}
		if s.nak {
			reply.Options[dhcp4.OptMessageType] = []byte{byte(dhcp4.Nak)}
		}
		if s.ack != nil {
			s.ack(&reply)
		}
	}
	s.replies <- reply.Encode()
	return nil
}

func (s *server) Recv(ctx context.Context) ([]byte, error) {
	select {
	case b := <-s.replies:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *server) received() []sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sent(nil), s.got...)
}

func (s *server) types() []dhcp4.MessageType {
	var out []dhcp4.MessageType
	for _, r := range s.received() {
		out = append(out, r.msg.Type())
	}
	return out
}

type handler struct {
	mu      sync.Mutex
	bound   []dhcp4.Lease
	expired []dhcp4.Lease
}

func (h *handler) Bound(l dhcp4.Lease) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bound = append(h.bound, l)
}

func (h *handler) Expired(l dhcp4.Lease) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expired = append(h.expired, l)
}

func (h *handler) counts() (bound, expired int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bound), len(h.expired)
}

func newClient(s *server, clock *fakeClock, h *handler) *dhcp4.Client {
	return &dhcp4.Client{HWAddr: hw, Transport: s, Clock: clock, Handler: h, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func acquire(t *testing.T, s *server, clock *fakeClock) dhcp4.Lease {
	t.Helper()
	l, err := newClient(s, clock, &handler{}).Acquire(t.Context())
	require.NoError(t, err)
	return l
}

// step moves the clock by a second at a time until cond holds, so that every timer the client
// sets is reached in order.
func step(t *testing.T, clock *fakeClock, cond func() bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		clock.advance(time.Second)
		return cond()
	}, 20*time.Second, time.Millisecond)
}

func TestClient_AcquireRunsDiscoverOfferRequestAckAndReadsTheLease(t *testing.T) {
	s := newServer()

	l := acquire(t, s, newClock())

	assert.Equal(t, []dhcp4.MessageType{dhcp4.Discover, dhcp4.Request}, s.types())
	assert.Equal(t, netip.MustParsePrefix("10.0.2.15/24"), l.Address)
	assert.Equal(t, serverIP, l.Router)
	assert.Equal(t, []netip.Addr{serverIP, netip.MustParseAddr("1.1.1.1")}, l.DNS)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.2.3")}, l.NTP)
	assert.Equal(t, serverIP, l.Server)
	assert.Equal(t, 1000*time.Second, l.Duration)
	assert.Equal(t, 500*time.Second, l.T1, "T1 is half the lease unless the server says otherwise")
	assert.Equal(t, 875*time.Second, l.T2, "T2 is seven eighths of it")
	assert.Equal(t, epoch, l.Acquired)

	got := s.received()
	assert.True(t, got[0].msg.Broadcast, "no address yet: ask for a broadcast reply")
	assert.False(t, got[0].from.IsValid())
	assert.False(t, got[0].to.IsValid(), "to the broadcast address")
	assert.Equal(t, leaseIP.AsSlice(), got[1].msg.Options[dhcp4.OptRequestedIP])
	assert.Equal(t, serverIP.AsSlice(), got[1].msg.Options[dhcp4.OptServerID])
	assert.Equal(t, got[0].msg.XID, got[1].msg.XID)
}

func TestClient_AcquireTakesTheTimesOfTheServer(t *testing.T) {
	s := newServer()
	s.ack = func(m *dhcp4.Message) {
		m.Options[dhcp4.OptRenewalTime] = binary.BigEndian.AppendUint32(nil, 100)
		m.Options[dhcp4.OptRebindingTime] = binary.BigEndian.AppendUint32(nil, 200)
	}

	l := acquire(t, s, newClock())

	assert.Equal(t, 100*time.Second, l.T1)
	assert.Equal(t, 200*time.Second, l.T2)
}

func TestClient_AcquireIgnoresAnOfferThatLacksWhatALeaseNeeds(t *testing.T) {
	tests := []struct {
		name string
		drop byte
	}{
		{"a subnet mask", dhcp4.OptSubnetMask},
		{"a server identifier", dhcp4.OptServerID},
		{"a lease time", dhcp4.OptLeaseTime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer()
			s.offer = func(m *dhcp4.Message) { delete(m.Options, tt.drop) }
			ctx, cancel := context.WithCancel(t.Context())
			clock := newClock()
			done := make(chan error, 1)
			go func() { _, err := newClient(s, clock, &handler{}).Acquire(ctx); done <- err }()

			require.Eventually(t, func() bool { return len(s.received()) >= 1 }, 5*time.Second, time.Millisecond)
			step(t, clock, func() bool { return len(s.received()) >= 2 })
			cancel()

			assert.ErrorIs(t, <-done, context.Canceled)
			for _, ty := range s.types() {
				assert.Equal(t, dhcp4.Discover, ty, "a request follows only an offer that is whole")
			}
		})
	}

	// The control: the same server with the offer whole leases.
	acquire(t, newServer(), newClock())
}

func TestClient_AcquireStartsAgainAfterANak(t *testing.T) {
	s := newServer()
	s.nak = true
	clock := newClock()
	done := make(chan dhcp4.Lease, 1)
	go func() {
		l, _ := newClient(s, clock, &handler{}).Acquire(t.Context())
		done <- l
	}()

	require.Eventually(t, func() bool { return len(s.received()) >= 2 }, 5*time.Second, time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	assert.Len(t, s.received(), 2, "after a refusal the client waits before it asks again")
	s.mu.Lock()
	s.nak = false
	s.mu.Unlock()
	clock.advance(4 * time.Second)

	l := <-done
	assert.Equal(t, leaseIP, l.Address.Addr())
	assert.Equal(t, []dhcp4.MessageType{dhcp4.Discover, dhcp4.Request, dhcp4.Discover, dhcp4.Request}, s.types())
}

func TestClient_AcquireRetransmitsWithADoublingTimeout(t *testing.T) {
	s := newServer()
	answer := false
	s.dropWhen = func(dhcp4.Message, sent) bool { return !answer }
	clock := newClock()
	done := make(chan error, 1)
	go func() { _, err := newClient(s, clock, &handler{}).Acquire(t.Context()); done <- err }()

	require.Eventually(t, func() bool { return len(s.received()) == 1 }, 5*time.Second, time.Millisecond)
	clock.advance(4 * time.Second)
	require.Eventually(t, func() bool { return len(s.received()) == 2 }, 5*time.Second, time.Millisecond)
	clock.advance(7 * time.Second)
	time.Sleep(20 * time.Millisecond)
	assert.Len(t, s.received(), 2, "the second wait is 8 s: 7 s is not enough")
	clock.advance(time.Second)
	require.Eventually(t, func() bool { return len(s.received()) == 3 }, 5*time.Second, time.Millisecond)

	s.mu.Lock()
	answer = true
	s.mu.Unlock()
	clock.advance(16 * time.Second)
	require.NoError(t, <-done)
}

func TestClient_RunRenewsAtT1ByAskingTheServerThatGaveTheLease(t *testing.T) {
	s := newServer()
	clock := newClock()
	h := &handler{}
	lease := acquire(t, s, clock)
	go func() { _ = newClient(s, clock, h).Run(t.Context(), lease) }()

	step(t, clock, func() bool { b, _ := h.counts(); return b == 1 })

	renew := s.received()[2]
	assert.Equal(t, dhcp4.Request, renew.msg.Type())
	assert.Equal(t, leaseIP, renew.from, "from the leased address")
	assert.Equal(t, serverIP, renew.to, "unicast to the server")
	assert.Equal(t, leaseIP, renew.msg.CIAddr)
	assert.NotContains(t, renew.msg.Options, dhcp4.OptRequestedIP)
	assert.GreaterOrEqual(t, clock.Now().Sub(epoch), 500*time.Second, "not before T1")
	assert.Less(t, clock.Now().Sub(epoch), 600*time.Second)
	_, expired := h.counts()
	assert.Zero(t, expired)
}

func TestClient_RunRebindsByBroadcastAtT2WhenTheServerDoesNotAnswerTheRenewal(t *testing.T) {
	s := newServer()
	clock := newClock()
	h := &handler{}
	lease := acquire(t, s, clock)
	s.dropWhen = func(_ dhcp4.Message, r sent) bool { return r.to.IsValid() }
	go func() { _ = newClient(s, clock, h).Run(t.Context(), lease) }()

	step(t, clock, func() bool { b, _ := h.counts(); return b == 1 })

	got := s.received()[2:]
	require.GreaterOrEqual(t, len(got), 2)
	assert.Equal(t, serverIP, got[0].to, "the renewal is unicast")
	last := got[len(got)-1]
	assert.False(t, last.to.IsValid(), "the rebinding is a broadcast")
	assert.Equal(t, leaseIP, last.from)
	assert.GreaterOrEqual(t, clock.Now().Sub(epoch), 875*time.Second, "not before T2")
	_, expired := h.counts()
	assert.Zero(t, expired, "the lease did not expire")
}

func TestClient_RunRemovesTheAddressWhenTheLeaseExpiresAndAcquiresAgain(t *testing.T) {
	s := newServer()
	clock := newClock()
	h := &handler{}
	lease := acquire(t, s, clock)
	var mu sync.Mutex
	silent := true
	s.dropWhen = func(m dhcp4.Message, _ sent) bool {
		mu.Lock()
		defer mu.Unlock()
		return silent
	}
	go func() { _ = newClient(s, clock, h).Run(t.Context(), lease) }()

	step(t, clock, func() bool { _, e := h.counts(); return e == 1 })

	assert.GreaterOrEqual(t, clock.Now().Sub(epoch), 1000*time.Second, "not before the lease ends")
	h.mu.Lock()
	assert.Equal(t, lease.Address, h.expired[0].Address)
	h.mu.Unlock()
	b, _ := h.counts()
	assert.Zero(t, b, "nothing bound while the server is silent")

	mu.Lock()
	silent = false
	mu.Unlock()
	step(t, clock, func() bool { b, _ := h.counts(); return b == 1 })
}

func TestClient_RunDropsTheLeaseAServerRefusesAtRenewal(t *testing.T) {
	s := newServer()
	clock := newClock()
	h := &handler{}
	lease := acquire(t, s, clock)
	s.mu.Lock()
	s.nak = true
	s.mu.Unlock()
	go func() { _ = newClient(s, clock, h).Run(t.Context(), lease) }()

	step(t, clock, func() bool { _, e := h.counts(); return e == 1 })
}

func TestClient_RunStopsWithItsContext(t *testing.T) {
	s := newServer()
	clock := newClock()
	lease := acquire(t, s, clock)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- newClient(s, clock, &handler{}).Run(ctx, lease) }()

	cancel()

	assert.ErrorIs(t, <-done, context.Canceled)
}

func TestMessage_RoundTrips(t *testing.T) {
	in := dhcp4.Message{
		Reply: true, XID: 0xdeadbeef, Broadcast: true,
		CIAddr: netip.MustParseAddr("10.0.0.1"), YIAddr: netip.MustParseAddr("10.0.0.2"), CHAddr: hw,
		Options: map[byte][]byte{dhcp4.OptMessageType: {byte(dhcp4.Ack)}, dhcp4.OptDNS: {1, 1, 1, 1, 8, 8, 8, 8}},
	}

	b := in.Encode()
	out, err := dhcp4.Decode(b)

	require.NoError(t, err)
	assert.Equal(t, in, out)
	assert.GreaterOrEqual(t, len(b), 300)
	assert.Equal(t, dhcp4.Ack, out.Type())
}

func TestDecode_Refuses(t *testing.T) {
	good := dhcp4.Message{XID: 1, CHAddr: hw, Options: map[byte][]byte{dhcp4.OptMessageType: {1}}}.Encode()
	_, err := dhcp4.Decode(good)
	require.NoError(t, err, "control: a whole message decodes")
	cut := append([]byte(nil), good...)
	cut[236+4], cut[236+5] = dhcp4.OptDNS, 200
	noCookie := append([]byte(nil), good...)
	noCookie[236] = 0
	tests := []struct {
		name string
		b    []byte
	}{
		{"a short message", good[:100]},
		{"a message without the magic cookie", noCookie},
		{"an option that runs past the end", cut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := dhcp4.Decode(tt.b)

			assert.Error(t, err)
		})
	}
}
