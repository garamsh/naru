package routes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// DefaultMaxAge is how long a proxy forwards on a table after its host took it, the proxy's own
// default (--route-max-age).
const DefaultMaxAge = time.Hour

// DefaultInterval is the pause between pushes when Config.Interval is zero, the network
// reconciler's.
const DefaultInterval = 10 * time.Second

// Source reads what the table is made of at one revision of orch's store, which it returns.
type Source interface {
	Routing(ctx context.Context) ([]machines.Record, []machines.Alias, int64, error)
}

// Hosts is the host API as the push needs it.
type Hosts interface {
	// Proxies are the admitted hosts that answer and report a proxy.
	Proxies(ctx context.Context) ([]string, error)
	// Admitted are the IDs of every admitted host, answering or not.
	Admitted(ctx context.Context) ([]string, error)
	// Push puts the table to the host, and returns the revision its proxy has loaded.
	Push(ctx context.Context, host string, t hostapi.RoutesRequest) (int64, error)
}

// Ack is the highest revision a host answered its proxy had loaded, and when the leader had it.
type Ack struct {
	Loaded int64     `json:"loaded"`
	At     time.Time `json:"at"`
}

// Acks keeps each proxy host's Ack, under /naru/orch/route-acks/<host>.
type Acks interface {
	Acks(ctx context.Context) (map[string]Ack, error)
	// Track records an Ack of revision 0 for a host that has none, before the host is first sent
	// a table, so the hosts a delete waits on include every one that may hold a table.
	Track(ctx context.Context, host string) error
	// Raise keeps a as the host's Ack when it is above the one kept.
	Raise(ctx context.Context, host string, a Ack) error
}

// Config is what the reconciler runs over.
type Config struct {
	Source Source
	Hosts  Hosts
	Acks   Acks
	// Zone is the zone of the stable names, orch's --machine-zone.
	Zone  string
	Clock machines.Clock
	// Interval is the pause between pushes, orch's --network-interval; Wake, when not nil, returns
	// a channel that is sent on after each write that may change the table, for a push at once.
	Interval time.Duration
	Wake     func(ctx context.Context) <-chan struct{}
	// MaxAge is the proxies' --route-max-age; DefaultMaxAge when zero. Interval is DefaultInterval
	// when zero.
	MaxAge time.Duration
	Log    *slog.Logger
}

// Reconciler is the leader's push of the routing table.
type Reconciler struct {
	cfg Config

	mu sync.Mutex
	// dropped are the routes left out and already logged, by hostname, with the reason logged.
	dropped map[string]string
}

// New returns the reconciler of cfg.
func New(cfg Config) *Reconciler {
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = DefaultMaxAge
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Reconciler{cfg: cfg, dropped: map[string]string{}}
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

// Reconcile derives the table and puts it to every proxy host at once, each push bounded by
// hostapi.MaxRouteDelay, and raises each host's Ack to the revision its proxy has loaded.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	recs, aliases, rev, err := r.cfg.Source.Routing(ctx)
	if err != nil {
		return fmt.Errorf("read the Machines and aliases: %w", err)
	}
	routes, dropped := Table(r.cfg.Zone, recs, aliases)
	r.logDropped(dropped)
	proxies, err := r.cfg.Hosts.Proxies(ctx)
	if err != nil {
		return fmt.Errorf("list the proxy hosts: %w", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(proxies))
	for i, h := range proxies {
		wg.Go(func() { errs[i] = r.push(ctx, h, rev, routes) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (r *Reconciler) push(ctx context.Context, host string, rev int64, routes []hostapi.Route) error {
	if err := r.cfg.Acks.Track(ctx, host); err != nil {
		return fmt.Errorf("track %s: %w", host, err)
	}
	pctx, cancel := context.WithTimeout(ctx, hostapi.MaxRouteDelay)
	defer cancel()
	now := r.cfg.Clock.Now()
	loaded, err := r.cfg.Hosts.Push(pctx, host, hostapi.RoutesRequest{Revision: rev, SentAt: now, Routes: routes})
	if err != nil {
		// A late table is refused with the difference the host saw, which this logs.
		return fmt.Errorf("push revision %d to %s: %w", rev, host, err)
	}
	if err := r.cfg.Acks.Raise(ctx, host, Ack{Loaded: loaded, At: now}); err != nil {
		return fmt.Errorf("keep %s's ack: %w", host, err)
	}
	return nil
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
// that has not, MaxAge and hostapi.RouteAllowance have passed since at, after which it forwards on
// no table taken before rev (ADR 0112 item 6). A host removed from the inventory leaves the set.
func (r *Reconciler) Delivered(ctx context.Context, rev int64, at time.Time) (bool, error) {
	acks, err := r.cfg.Acks.Acks(ctx)
	if err != nil {
		return false, err
	}
	admitted, err := r.cfg.Hosts.Admitted(ctx)
	if err != nil {
		return false, err
	}
	expired := !r.cfg.Clock.Now().Before(at.Add(r.cfg.MaxAge + hostapi.RouteAllowance))
	for _, h := range admitted {
		if a, ok := acks[h]; ok && a.Loaded < rev && !expired {
			return false, nil
		}
	}
	return true, nil
}
