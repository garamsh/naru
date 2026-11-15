package dhcp4

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"
)

// ErrTimeout is what a Transport's Recv returns when its context ends without a packet, and what
// the client treats an unanswered request as.
var ErrTimeout = errors.New("dhcp4: no reply")

var (
	errNak     = errors.New("dhcp4: the server refused the request")
	errExpired = errors.New("dhcp4: the lease expired")
)

// Transport moves DHCP messages. Send gives a message to the network with the given IPv4 source
// (the zero Addr before the client has an address) and destination (the zero Addr for the
// broadcast address); Recv returns the next message addressed to the client, or ctx's error.
type Transport interface {
	Send(from, to netip.Addr, msg []byte) error
	Recv(ctx context.Context) ([]byte, error)
}

// Clock is the time the client waits on.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// Handler is told what the client changes on its own after the first lease.
type Handler interface {
	// Bound is called when a renewal, a rebinding or a new acquisition gives a lease.
	Bound(Lease)
	// Expired is called when the lease is gone: it expired, or the server refused it. The address
	// of the lease must be removed.
	Expired(Lease)
}

// Lease is what an ACK gave the client.
type Lease struct {
	Address netip.Prefix
	// Router is the first of the routers, the zero Addr when the server named none.
	Router   netip.Addr
	DNS      []netip.Addr
	NTP      []netip.Addr
	Server   netip.Addr
	Duration time.Duration
	// T1 and T2 are the times after Acquired at which the client renews and rebinds.
	T1, T2   time.Duration
	Acquired time.Time
}

// Expires is when the lease ends.
func (l Lease) Expires() time.Time { return l.Acquired.Add(l.Duration) }

const (
	firstTimeout  = 4 * time.Second
	maxTimeout    = 64 * time.Second
	attemptTime   = 4 * time.Second
	minRetryDelay = 60 * time.Second
)

// Client runs the DHCPv4 exchange for one interface.
type Client struct {
	HWAddr    net.HardwareAddr
	Transport Transport
	Clock     Clock
	Handler   Handler
	Log       *slog.Logger
}

// Acquire runs DISCOVER, OFFER, REQUEST and ACK until a server gives a lease or ctx ends. It
// retransmits with a timeout that doubles from 4 s to 64 s.
func (c *Client) Acquire(ctx context.Context) (Lease, error) {
	timeout := firstTimeout
	for {
		l, err := c.acquireOnce(ctx, timeout)
		switch {
		case err == nil:
			return l, nil
		case ctx.Err() != nil:
			return Lease{}, ctx.Err()
		case errors.Is(err, ErrTimeout), errors.Is(err, errNak):
			c.Log.Info("no lease yet; trying again", "reason", err.Error())
			if errors.Is(err, errNak) {
				// A refusal comes at once, so waiting is what keeps the client from asking in a loop.
				select {
				case <-c.Clock.After(timeout):
				case <-ctx.Done():
					return Lease{}, ctx.Err()
				}
			}
			timeout = min(timeout*2, maxTimeout)
		default:
			return Lease{}, err
		}
	}
}

func (c *Client) acquireOnce(ctx context.Context, timeout time.Duration) (Lease, error) {
	discover := c.message(Discover)
	discover.Broadcast = true
	offer, err := c.exchange(ctx, netip.Addr{}, netip.Addr{}, discover, timeout, func(m Message) bool {
		if m.Type() != Offer {
			return false
		}
		_, err := leaseOf(m, c.Clock.Now())
		if err != nil {
			c.Log.Warn("offer refused", "err", err)
		}
		return err == nil
	})
	if err != nil {
		return Lease{}, err
	}
	offered, _ := leaseOf(offer, c.Clock.Now())

	request := c.message(Request)
	request.XID = discover.XID
	request.Broadcast = true
	request.Options[OptRequestedIP] = offered.Address.Addr().AsSlice()
	request.Options[OptServerID] = offered.Server.AsSlice()
	sent := c.Clock.Now()
	ack, err := c.exchange(ctx, netip.Addr{}, netip.Addr{}, request, timeout, ackOrNak)
	if err != nil {
		return Lease{}, err
	}
	if ack.Type() == Nak {
		return Lease{}, errNak
	}
	l, err := leaseOf(ack, sent)
	if err != nil {
		c.Log.Warn("ack refused", "err", err)
		return Lease{}, ErrTimeout
	}
	return l, nil
}

// Run keeps lease for as long as ctx lasts: it renews at T1 by asking the server that gave it, then
// rebinds at T2 by asking any server, and when the lease expires or is refused it tells the
// handler and acquires a new one. It returns ctx's error, or the error of a transport that
// failed.
func (c *Client) Run(ctx context.Context, lease Lease) error {
	for {
		next, err := c.keep(ctx, lease)
		switch {
		case err == nil:
			lease = next
			c.Handler.Bound(lease)
			continue
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, errExpired), errors.Is(err, errNak):
			c.Log.Warn("lease lost; its address and route are removed", "address", lease.Address, "reason", err.Error())
			c.Handler.Expired(lease)
		default:
			return err
		}
		lease, err = c.Acquire(ctx)
		if err != nil {
			return err
		}
		c.Handler.Bound(lease)
	}
}

// keep returns the lease that renewing or rebinding gave, or errExpired or errNak.
func (c *Client) keep(ctx context.Context, lease Lease) (Lease, error) {
	t1, t2, end := lease.Acquired.Add(lease.T1), lease.Acquired.Add(lease.T2), lease.Expires()
	if err := c.sleepUntil(ctx, t1); err != nil {
		return Lease{}, err
	}
	for _, stage := range []struct {
		name  string
		to    netip.Addr
		until time.Time
	}{
		{"renewing", lease.Server, t2},
		{"rebinding", netip.Addr{}, end},
	} {
		for c.Clock.Now().Before(stage.until) {
			sent := c.Clock.Now()
			req := c.message(Request)
			req.CIAddr = lease.Address.Addr()
			ack, err := c.exchange(ctx, lease.Address.Addr(), stage.to, req, attemptTime, ackOrNak)
			switch {
			case err == nil && ack.Type() == Nak:
				return Lease{}, errNak
			case err == nil:
				l, lerr := leaseOf(ack, sent)
				if lerr != nil {
					c.Log.Warn("ack refused", "err", lerr)
					break
				}
				c.Log.Info("lease renewed", "stage", stage.name, "address", l.Address)
				return l, nil
			case errors.Is(err, ErrTimeout):
			default:
				return Lease{}, err
			}
			now := c.Clock.Now()
			if !now.Before(stage.until) {
				break
			}
			retry := max(stage.until.Sub(now)/2, minRetryDelay)
			if err := c.sleepUntil(ctx, minTime(now.Add(retry), stage.until)); err != nil {
				return Lease{}, err
			}
		}
	}
	return Lease{}, errExpired
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (c *Client) sleepUntil(ctx context.Context, t time.Time) error {
	d := t.Sub(c.Clock.Now())
	if d <= 0 {
		return nil
	}
	select {
	case <-c.Clock.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func ackOrNak(m Message) bool { return m.Type() == Ack || m.Type() == Nak }

// message is a request of the given type with a new transaction.
func (c *Client) message(t MessageType) Message {
	var x [4]byte
	_, _ = rand.Read(x[:])
	return Message{
		XID:    binary.BigEndian.Uint32(x[:]),
		CHAddr: c.HWAddr,
		Options: map[byte][]byte{
			OptMessageType:   {byte(t)},
			OptClientID:      append([]byte{hardwareEthernet}, c.HWAddr...),
			OptParameterList: {OptSubnetMask, OptRouter, OptDNS, OptNTP, OptLeaseTime, OptRenewalTime, OptRebindingTime},
		},
	}
}

// exchange sends req and returns the first reply that accept takes, or ErrTimeout after timeout.
func (c *Client) exchange(ctx context.Context, from, to netip.Addr, req Message, timeout time.Duration, accept func(Message) bool) (Message, error) {
	expire := c.Clock.After(timeout)
	if err := c.Transport.Send(from, to, req.Encode()); err != nil {
		c.Log.Warn("send failed", "type", req.Type(), "err", err)
		select {
		case <-expire:
			return Message{}, ErrTimeout
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
	wait, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-expire:
			cancel()
		case <-wait.Done():
		}
	}()
	for {
		b, err := c.Transport.Recv(wait)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return Message{}, ctx.Err()
			case wait.Err() != nil, errors.Is(err, ErrTimeout):
				return Message{}, ErrTimeout
			}
			return Message{}, fmt.Errorf("dhcp4: receive: %w", err)
		}
		m, err := Decode(b)
		if err != nil || !m.Reply || m.XID != req.XID || !bytes.Equal(m.CHAddr, c.HWAddr) {
			continue
		}
		if accept(m) {
			return m, nil
		}
	}
}

// leaseOf reads the lease an OFFER or an ACK gives, acquired at the given time.
func leaseOf(m Message, acquired time.Time) (Lease, error) {
	if !m.YIAddr.IsValid() || m.YIAddr.IsUnspecified() {
		return Lease{}, errors.New("dhcp4: the message gives no address")
	}
	mask := net.IPMask(m.Options[OptSubnetMask])
	ones, bits := mask.Size()
	if len(mask) != 4 || bits == 0 {
		return Lease{}, errors.New("dhcp4: the message has no valid subnet mask")
	}
	server := addrs(m.Options[OptServerID])
	if len(server) != 1 {
		return Lease{}, errors.New("dhcp4: the message has no server identifier")
	}
	secs := m.Options[OptLeaseTime]
	if len(secs) != 4 || binary.BigEndian.Uint32(secs) == 0 {
		return Lease{}, errors.New("dhcp4: the message has no lease time")
	}
	l := Lease{
		Address:  netip.PrefixFrom(m.YIAddr, ones),
		Server:   server[0],
		DNS:      addrs(m.Options[OptDNS]),
		NTP:      addrs(m.Options[OptNTP]),
		Duration: seconds(secs),
		Acquired: acquired,
	}
	if r := addrs(m.Options[OptRouter]); len(r) > 0 {
		l.Router = r[0]
	}
	l.T1, l.T2 = l.Duration/2, l.Duration/8*7
	t1, t2 := m.Options[OptRenewalTime], m.Options[OptRebindingTime]
	if len(t1) == 4 && len(t2) == 4 {
		if a, b := seconds(t1), seconds(t2); 0 < a && a < b && b < l.Duration {
			l.T1, l.T2 = a, b
		}
	}
	return l, nil
}

func seconds(v []byte) time.Duration {
	return time.Duration(binary.BigEndian.Uint32(v)) * time.Second
}
