package network

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// The timing of the announcement (ADR 0085 item 6): a gratuitous ARP when the address is acquired
// and every AnnounceEvery for AnnounceFor after.
const (
	AnnounceEvery = 5 * time.Second
	AnnounceFor   = 30 * time.Second
)

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
	// Idle is how long a proxied connection may be silent in a direction; zero is DefaultIdle.
	Idle time.Duration

	mu       sync.Mutex
	ctx      context.Context
	held     map[netip.Addr]*held
	slots    *limiter
	strays   []netip.Addr
	restored bool
}

// held is an address the host owns and what is open for it.
type held struct {
	acquired     time.Time
	deadline     time.Time
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
// leases that ran out when it loaded, so none of them is served. A failure of one address does not
// stop the others, and Tick tries it again. ctx ends the proxies.
func (x *Exposer) Restore(ctx context.Context) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.ctx, x.held, x.slots = ctx, map[netip.Addr]*held{}, newLimiter(maxProxyConns, maxPerSource)
	x.restored = true
	left, err := x.Link.Held()
	if err != nil {
		return errors.Join(fmt.Errorf("list the addresses on the LAN interface: %w", err), x.joined(x.sync()))
	}
	want := map[netip.Addr]bool{}
	for _, e := range x.Manager.Exposures() {
		want[e.Address] = true
	}
	var errs []error
	for _, a := range left {
		if !want[a] {
			if err := x.Link.Remove(a); err != nil {
				x.strays = append(x.strays, a)
				errs = append(errs, fmt.Errorf("remove %s from the LAN interface: %w", a, err))
			}
		}
	}
	return errors.Join(append(errs, x.joined(x.sync()))...)
}

// joined makes one error of the errors of a pass, in the order of the addresses.
func (x *Exposer) joined(errs map[netip.Addr]error) error {
	var all []error
	for _, a := range slices.SortedFunc(maps.Keys(errs), netip.Addr.Compare) {
		all = append(all, errs[a])
	}
	return errors.Join(all...)
}

// Put takes the lease or renews it, for lease from now. A lease that is new and that the host cannot
// open is released and the error returned, so the host holds no rule for an address it does not own.
// A renewal that fails keeps the lease it renews: the host serves it until its deadline.
func (x *Exposer) Put(ctx context.Context, e Exposure, lease time.Duration) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.restored {
		return fmt.Errorf("the exposures were not restored")
	}
	renewal := slices.ContainsFunc(x.Manager.Exposures(), func(o Exposure) bool { return o.Address == e.Address })
	if !renewal {
		if err := x.Link.Check(e.Address); err != nil {
			return err
		}
		if err := x.Link.Probe(e.Address); err != nil {
			return err
		}
	}
	e.Deadline = x.now().Add(lease)
	if err := x.Manager.PutExposure(ctx, e); err != nil {
		return err
	}
	err := x.sync()[e.Address]
	if err != nil && !renewal {
		if derr := x.Manager.DeleteExposure(ctx, e.Address); derr != nil {
			err = fmt.Errorf("%w; and the lease could not be released: %v", err, derr)
		}
		_ = x.sync()
	}
	return err
}

// Delete releases the lease on a. An absent one is not an error.
func (x *Exposer) Delete(ctx context.Context, a netip.Addr) error {
	if err := x.Manager.DeleteExposure(ctx, a); err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.joined(x.sync())
}

// Exposures returns the leases the host holds, ordered by address.
func (x *Exposer) Exposures() []Exposure { return x.Manager.Exposures() }

// Tick makes one pass: it drops the leases that ran out, makes the addresses and listeners what the
// leases say, and announces the addresses that are due. A lease that fails does not stop the others;
// the failures are returned together.
func (x *Exposer) Tick(ctx context.Context) error {
	var errs []error
	gone, err := x.Manager.ExpireExposures(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("drop the leases that ran out: %w", err))
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(gone) > 0 {
		x.Log.Info("leases ran out", "addresses", fmt.Sprint(gone))
	}
	errs = append(errs, x.retryStrays())
	errs = append(errs, x.joined(x.sync()))
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
	return errors.Join(errs...)
}

// retryStrays takes off the interface the addresses of a former run that Restore could not.
func (x *Exposer) retryStrays() error {
	var errs []error
	var left []netip.Addr
	for _, a := range x.strays {
		if err := x.Link.Remove(a); err != nil {
			left = append(left, a)
			errs = append(errs, fmt.Errorf("remove %s from the LAN interface: %w", a, err))
		}
	}
	x.strays = left
	return errors.Join(errs...)
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

// sync makes held what the Manager holds, one address at a time, and returns the error of each
// address that failed. The Exposer's lock is held.
func (x *Exposer) sync() map[netip.Addr]error {
	errs := map[netip.Addr]error{}
	want := map[netip.Addr]Exposure{}
	for _, e := range x.Manager.Exposures() {
		want[e.Address] = e
	}
	for a, h := range x.held {
		if _, ok := want[a]; !ok {
			h.close()
			if err := x.Link.Remove(a); err != nil {
				errs[a] = fmt.Errorf("remove %s from the LAN interface: %w", a, err)
				continue
			}
			delete(x.held, a)
		}
	}
	for a, e := range want {
		now := x.now()
		h := x.held[a]
		if h == nil || !h.deadline.Equal(e.Deadline) {
			// The kernel removes the address when the lease runs out, also when hostd does not run.
			if err := x.Link.Add(a, e.Deadline.Sub(now)); err != nil {
				errs[a] = fmt.Errorf("add %s to the LAN interface: %w", a, err)
				continue
			}
			if h == nil {
				h = &held{acquired: now, nextAnnounce: now, proxies: map[int]*proxy{}}
				x.held[a] = h
			}
			h.deadline = e.Deadline
		}
		if err := x.proxies(h, e); err != nil {
			errs[a] = err
		}
	}
	return errs
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
		px := &proxy{ln: ln, nodePort: p.NodePort, dial: x.dial, limit: x.slots, cancel: cancel, log: x.Log, idle: x.Idle}
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
