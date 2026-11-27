package routes

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/orch/internal/machines"
)

// DefaultInterval is the pause between pushes when Config.Interval is zero, the network
// reconciler's.
const DefaultInterval = 10 * time.Second

// The fleet's events of the push.
const (
	// EventProxyNotGranted is a host that reports a proxy without the operator's grant: it is sent
	// no table and no peer names it a proxy. One per host for each leader.
	EventProxyNotGranted = "route.proxy-not-granted"
	// EventMaxAgeClamped is a host that reported a max age outside (0, wire.ProxyMaxRouteMaxAge],
	// which is taken as the longest. One per host and value for each leader.
	EventMaxAgeClamped = "route.max-age-clamped"
)

// Source reads what the table is made of at one revision of orch's store, which it returns.
type Source interface {
	Routing(ctx context.Context) ([]machines.Record, []machines.Alias, int64, error)
}

// Hosts is the host API as the push needs it.
type Hosts interface {
	// Proxies probes the admitted hosts.
	Proxies(ctx context.Context) (Probe, error)
	// Admitted are the IDs of every admitted host, answering or not.
	Admitted(ctx context.Context) ([]string, error)
	// Push puts the table to the host, and returns the revision its proxy has loaded.
	Push(ctx context.Context, host string, t hostapi.RoutesRequest) (int64, error)
}

// Probe is what the admitted hosts that answered report of a proxy.
type Probe struct {
	// Proxies hold the operator's grant and report a proxy; they are sent the table.
	Proxies []Proxy
	// Ungranted report a proxy without the grant, and NoProxy answer with none.
	Ungranted, NoProxy []string
}

// Proxy is a host that holds the grant and reports a proxy, with the proxy's --route-max-age in
// seconds as the host reports it, zero when it reports none.
type Proxy struct {
	ID            string
	MaxAgeSeconds int64
}

// Ack is the highest revision a host answered its proxy had loaded, the longest max age the host
// reported for its proxy, and when the leader last sent it a table, by its clock.
type Ack struct {
	Loaded   int64         `json:"loaded"`
	MaxAge   time.Duration `json:"maxAge"`
	LastSent time.Time     `json:"lastSent"`
}

// Acks keeps each proxy host's Ack, under /naru/orch/route-acks/<host>.
type Acks interface {
	Acks(ctx context.Context) (map[string]Ack, error)
	// Track records, before the host is sent a table, an Ack of revision 0 for a host that has
	// none, raises the kept MaxAge to maxAge and sets LastSent to sent, so the hosts a delete waits
	// on include every one that may hold a table, each with the longest it may forward on one.
	Track(ctx context.Context, host string, maxAge time.Duration, sent time.Time) error
	// Raise keeps loaded as the host's when it is above the one kept.
	Raise(ctx context.Context, host string, loaded int64) error
	// Forget deletes the host's Ack when it is still a as read.
	Forget(ctx context.Context, host string, a Ack) error
}

// Config is what the reconciler runs over.
type Config struct {
	Source Source
	Hosts  Hosts
	Acks   Acks
	// Zone is the zone of the stable names, orch's --machine-zone.
	Zone  string
	Clock machines.Clock
	// Interval is the pause between pushes, orch's --network-interval, DefaultInterval when zero;
	// Wake, when not nil, returns a channel that is sent on after each write that may change the
	// table, for a push at once.
	Interval time.Duration
	Wake     func(ctx context.Context) <-chan struct{}
	// Fleet takes the push's events; nil logs them only.
	Fleet machines.Appender
	Log   *slog.Logger
}

// Reconciler is the leader's push of the routing table.
type Reconciler struct {
	cfg Config

	mu sync.Mutex
	// dropped are the routes left out and already logged, by hostname, with the reason logged.
	dropped map[string]string
	// noted are the events already appended this term, by type, host and value.
	noted map[string]bool
}

// New returns the reconciler of cfg.
func New(cfg Config) *Reconciler {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Reconciler{cfg: cfg, dropped: map[string]string{}, noted: map[string]bool{}}
}

// Run pushes every interval and after each wake until ctx ends.
func (r *Reconciler) Run(ctx context.Context) error {
	var wake <-chan struct{}
	if r.cfg.Wake != nil {
		wake = r.cfg.Wake(ctx)
	}
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Log.Error("routing push failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case _, ok := <-wake:
			if !ok {
				wake = nil
			}
		}
	}
}

// Reconcile probes the hosts, records each proxy host's Ack with the time of sending, and only
// then reads the table and puts it to every proxy host at once, each push bounded by
// hostapi.MaxRouteDelay, raising each host's Ack to the revision its proxy has loaded. So a table
// older than a commit is sent with a time before the commit, and a host that may take it is
// tracked before a delete after the commit asks (ADR 0112 item 6).
func (r *Reconciler) Reconcile(ctx context.Context) error {
	probe, err := r.cfg.Hosts.Proxies(ctx)
	if err != nil {
		return fmt.Errorf("probe the proxy hosts: %w", err)
	}
	for _, h := range probe.Ungranted {
		r.note(ctx, EventProxyNotGranted, h, "", "a host reports a proxy without the operator's grant: it is sent no table")
	}
	now := r.cfg.Clock.Now()
	var errs []error
	var proxies []Proxy
	for _, p := range probe.Proxies {
		if err := r.cfg.Acks.Track(ctx, p.ID, r.maxAge(ctx, p), now); err != nil {
			errs = append(errs, fmt.Errorf("track %s: %w", p.ID, err))
			continue
		}
		proxies = append(proxies, p)
	}
	if err := r.forget(ctx, append(probe.NoProxy, probe.Ungranted...), now); err != nil {
		errs = append(errs, err)
	}
	recs, aliases, rev, err := r.cfg.Source.Routing(ctx)
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("read the Machines and aliases: %w", err))...)
	}
	routes, dropped := Table(r.cfg.Zone, recs, aliases)
	r.logDropped(dropped)
	pushed := make([]error, len(proxies))
	var wg sync.WaitGroup
	for i, p := range proxies {
		wg.Go(func() {
			pushed[i] = r.push(ctx, p.ID, hostapi.RoutesRequest{Revision: rev, SentAt: now, Routes: routes})
		})
	}
	wg.Wait()
	return errors.Join(append(errs, pushed...)...)
}

func (r *Reconciler) push(ctx context.Context, host string, t hostapi.RoutesRequest) error {
	pctx, cancel := context.WithTimeout(ctx, hostapi.MaxRouteDelay)
	defer cancel()
	loaded, err := r.cfg.Hosts.Push(pctx, host, t)
	if err != nil {
		// A late table is refused with the difference the host saw, which this logs.
		return fmt.Errorf("push revision %d to %s: %w", t.Revision, host, err)
	}
	if err := r.cfg.Acks.Raise(ctx, host, loaded); err != nil {
		return fmt.Errorf("keep %s's ack: %w", host, err)
	}
	return nil
}

// maxAge is the max age the host reports for its proxy, wire.ProxyRouteMaxAge when it reports none,
// and wire.ProxyMaxRouteMaxAge, with an event, when it reports one outside (0, that bound].
func (r *Reconciler) maxAge(ctx context.Context, p Proxy) time.Duration {
	limit := int64(wire.ProxyMaxRouteMaxAge / time.Second)
	switch {
	case p.MaxAgeSeconds == 0:
		return wire.ProxyRouteMaxAge
	case p.MaxAgeSeconds < 0 || p.MaxAgeSeconds > limit:
		r.note(ctx, EventMaxAgeClamped, p.ID, strconv.FormatInt(p.MaxAgeSeconds, 10),
			"a host reports a route max age outside what a proxy may be given: it is taken as the longest")
		return wire.ProxyMaxRouteMaxAge
	}
	return time.Duration(p.MaxAgeSeconds) * time.Second
}

// forget deletes the Ack of each host that answered with no proxy, or one without the grant, once its last table has
// expired: past its max age and hostapi.RouteRelease after it was sent, its proxy forwards on none.
func (r *Reconciler) forget(ctx context.Context, hosts []string, now time.Time) error {
	if len(hosts) == 0 {
		return nil
	}
	acks, err := r.cfg.Acks.Acks(ctx)
	if err != nil {
		return fmt.Errorf("read the acks: %w", err)
	}
	var errs []error
	for _, h := range hosts {
		a, ok := acks[h]
		if ok && !now.Before(a.LastSent.Add(a.MaxAge+hostapi.RouteRelease)) {
			if err := r.cfg.Acks.Forget(ctx, h, a); err != nil {
				errs = append(errs, fmt.Errorf("forget %s's ack: %w", h, err))
			}
		}
	}
	return errors.Join(errs...)
}

// note appends the event once per type, host and value for this reconciler, and logs it.
func (r *Reconciler) note(ctx context.Context, typ, host, value, msg string) {
	key := typ + "\x00" + host + "\x00" + value
	r.mu.Lock()
	seen := r.noted[key]
	r.noted[key] = true
	r.mu.Unlock()
	if seen {
		return
	}
	r.cfg.Log.Warn(msg, "host", host, "value", value)
	if r.cfg.Fleet == nil {
		return
	}
	detail := map[string]string{}
	if value != "" {
		detail["reported"] = value
	}
	ev := orchapi.Event{Time: r.cfg.Clock.Now().UTC(), Type: typ, Subject: host, Detail: detail}
	if err := r.cfg.Fleet.AppendEvent(ctx, ev); err != nil {
		r.cfg.Log.Error("cannot append the event", "type", typ, "host", host, "err", err.Error())
		r.mu.Lock()
		delete(r.noted, key)
		r.mu.Unlock()
	}
}

// logDropped logs each route the table leaves out once for each reason, and forgets the ones that
// are back.
func (r *Reconciler) logDropped(dropped []Dropped) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := map[string]string{}
	for _, d := range dropped {
		now[d.Hostname] = d.Reason
		if r.dropped[d.Hostname] != d.Reason {
			r.cfg.Log.Warn("a route is left out of the table", "hostname", d.Hostname, "reason", d.Reason)
		}
	}
	r.dropped = now
}

// Delivered reports whether no proxy forwards on a table older than rev, which was committed at or
// before at: every admitted host that may hold a table has acknowledged rev or above, or, for one
// that has not, the longest max age it reported for its proxy and hostapi.RouteRelease have passed
// since at, after which it forwards on no table sent before rev (ADR 0112 item 6). A host removed
// from the inventory leaves the set.
func (r *Reconciler) Delivered(ctx context.Context, rev int64, at time.Time) (bool, error) {
	acks, err := r.cfg.Acks.Acks(ctx)
	if err != nil {
		return false, err
	}
	admitted, err := r.cfg.Hosts.Admitted(ctx)
	if err != nil {
		return false, err
	}
	now := r.cfg.Clock.Now()
	for _, h := range admitted {
		a, ok := acks[h]
		if !ok || a.Loaded >= rev {
			continue
		}
		if now.Before(at.Add(cmp.Or(a.MaxAge, wire.ProxyMaxRouteMaxAge) + hostapi.RouteRelease)) {
			return false, nil
		}
	}
	return true, nil
}
