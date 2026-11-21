package network

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

// The timing of the announcement (ADR 0085 item 6): a gratuitous ARP when the address is acquired
// and every AnnounceEvery for AnnounceFor after.
const (
	AnnounceEvery = 5 * time.Second
	AnnounceFor   = 30 * time.Second
)

// Link is the LAN interface: the addresses the host owns on it and its announcements.
type Link interface {
	// Add gives the interface the address as a /32; an address it holds already is not an error.
	Add(a netip.Addr) error
	// Remove takes the address off; an absent one is not an error.
	Remove(a netip.Addr) error
	// Held lists the addresses Add put on the interface and Remove has not taken off, including those
	// of an earlier run of the daemon.
	Held() ([]netip.Addr, error)
	// Announce sends an unsolicited ARP reply for the address.
	Announce(a netip.Addr) error
}

// Exposer makes the host's side of the exposures what the Manager holds: the addresses on the LAN
// interface, their announcements, and the listeners of the TCP proxy. The Manager holds the leases
// and their rules; the Exposer holds what the kernel and the sockets hold.
type Exposer struct {
	Manager *Manager
	Link    Link
	Log     *slog.Logger
	// Listen and Dial open the proxy's sockets; nil are net.Listen and a net.Dialer.
	Listen func(addr string) (net.Listener, error)
	Dial   func(ctx context.Context, addr string) (net.Conn, error)
	// Now is the clock of the announcements; nil is time.Now.
	Now func() time.Time

	mu       sync.Mutex
	ctx      context.Context
	held     map[netip.Addr]*held
	slots    chan struct{}
	restored bool
}

// held is an address the host owns and what is open for it.
type held struct {
	acquired     time.Time
	nextAnnounce time.Time
	proxies      map[int]*proxy
}

func (x *Exposer) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

// Restore makes the host hold what the Manager's state says: it takes off the interface every
// address a former run left and no lease holds, and brings up the rest. The Manager has dropped the
// leases that ran out when it loaded, so none of them is served. ctx ends the proxies.
func (x *Exposer) Restore(ctx context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ctx, x.held, x.slots = ctx, map[netip.Addr]*held{}, make(chan struct{}, maxProxyConns)
	left, err := x.Link.Held()
	if err != nil {
		return fmt.Errorf("list the addresses on the LAN interface: %w", err)
	}
	want := map[netip.Addr]bool{}
	for _, e := range x.Manager.Exposures() {
		want[e.Address] = true
	}
	for _, a := range left {
		if !want[a] {
			if err := x.Link.Remove(a); err != nil {
				return fmt.Errorf("remove %s from the LAN interface: %w", a, err)
			}
		}
	}
	x.restored = true
	return x.sync()
}

// Put takes the lease or renews it, for lease from now. When the host cannot open the address, the lease is released
// and the error returned, so the host holds no rule for an address it does not own.
func (x *Exposer) Put(ctx context.Context, e Exposure, lease time.Duration) error {
	e.Deadline = x.now().Add(lease)
	if err := x.Manager.PutExposure(ctx, e); err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if err := x.sync(); err != nil {
		if derr := x.Manager.DeleteExposure(ctx, e.Address); derr != nil {
			err = fmt.Errorf("%w; and the lease could not be released: %v", err, derr)
		}
		_ = x.sync()
		return err
	}
	return nil
}

// Delete releases the lease on a. An absent one is not an error.
func (x *Exposer) Delete(ctx context.Context, a netip.Addr) error {
	if err := x.Manager.DeleteExposure(ctx, a); err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.sync()
}

// Exposures returns the leases the host holds, ordered by address.
func (x *Exposer) Exposures() []Exposure { return x.Manager.Exposures() }

// Tick makes one pass: it drops the leases that ran out, then announces the addresses that are due.
func (x *Exposer) Tick(ctx context.Context) error {
	gone, err := x.Manager.ExpireExposures(ctx)
	if err != nil {
		return fmt.Errorf("drop the leases that ran out: %w", err)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(gone) > 0 {
		x.Log.Info("leases ran out", "addresses", fmt.Sprint(gone))
	}
	if err := x.sync(); err != nil {
		return err
	}
	now := x.now()
	for a, h := range x.held {
		if now.Before(h.nextAnnounce) || now.Sub(h.acquired) > AnnounceFor {
			continue
		}
		h.nextAnnounce = now.Add(AnnounceEvery)
		if err := x.Link.Announce(a); err != nil {
			x.Log.Error("cannot announce an address", "address", a.String(), "err", err.Error())
		}
	}
	return nil
}

// Run ticks every second until ctx ends. A failed pass is logged.
func (x *Exposer) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := x.Tick(ctx); err != nil && ctx.Err() == nil {
				x.Log.Error("cannot keep the exposures", "err", err.Error())
			}
		}
	}
}

// sync makes held what the Manager holds. The Exposer's lock is held.
func (x *Exposer) sync() error {
	if !x.restored {
		return fmt.Errorf("the exposures were not restored")
	}
	want := map[netip.Addr]Exposure{}
	for _, e := range x.Manager.Exposures() {
		want[e.Address] = e
	}
	var first error
	for a, h := range x.held {
		if _, ok := want[a]; !ok {
			h.close()
			if err := x.Link.Remove(a); err != nil && first == nil {
				first = fmt.Errorf("remove %s from the LAN interface: %w", a, err)
				continue
			}
			delete(x.held, a)
		}
	}
	for a, e := range want {
		h := x.held[a]
		if h == nil {
			if err := x.Link.Add(a); err != nil {
				if first == nil {
					first = fmt.Errorf("add %s to the LAN interface: %w", a, err)
				}
				continue
			}
			now := x.now()
			h = &held{acquired: now, nextAnnounce: now, proxies: map[int]*proxy{}}
			x.held[a] = h
		}
		if err := x.proxies(h, e); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// close closes the listeners and the connections of the address.
func (h *held) close() {
	for _, p := range h.proxies {
		p.cancel()
	}
}

// proxies opens the listeners of e's proxied ports and closes those it no longer has, and gives
// the open ones its backends.
func (x *Exposer) proxies(h *held, e Exposure) error {
	ports := map[int]ExposurePort{}
	for _, p := range e.proxied() {
		ports[p.Port] = p
	}
	for port, p := range h.proxies {
		if q, ok := ports[port]; !ok || q.NodePort != p.nodePort {
			p.cancel()
			delete(h.proxies, port)
		}
	}
	for port, p := range ports {
		if px, ok := h.proxies[port]; ok {
			px.setBackends(e.Backends)
			continue
		}
		ln, err := x.listen(netip.AddrPortFrom(e.Address, uint16(port)).String())
		if err != nil {
			return fmt.Errorf("listen on %s:%d: %w", e.Address, port, err)
		}
		ctx, cancel := context.WithCancel(x.ctx)
		px := &proxy{ln: ln, nodePort: p.NodePort, dial: x.dial, slots: x.slots, cancel: cancel}
		px.setBackends(e.Backends)
		h.proxies[port] = px
		go px.serve(ctx)
	}
	return nil
}

func (x *Exposer) listen(addr string) (net.Listener, error) {
	if x.Listen != nil {
		return x.Listen(addr)
	}
	return net.Listen("tcp4", addr)
}

func (x *Exposer) dial(ctx context.Context, addr string) (net.Conn, error) {
	if x.Dial != nil {
		return x.Dial(ctx, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp4", addr)
}
