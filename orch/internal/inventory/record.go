package inventory

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
)

// DefaultRecordInterval is the pause between the recorder's passes when none is given.
const DefaultRecordInterval = 10 * time.Second

// PrefixHosts is what the recorder reads of the hosts: the Machine prefixes each admitted host
// reports, by host ID. A host that did not answer is absent.
type PrefixHosts interface {
	MachinePrefixes(ctx context.Context) (map[string][]string, error)
}

// Recorder writes the Machine prefix every admitted host reports into the allocation records the
// pool reads, whether the host joined from the install seed (orch chose its prefix) or enrolled with
// a configuration of its own (it did). Whoever holds the leader lease runs one.
type Recorder struct {
	store    Store
	hosts    PrefixHosts
	clock    Clock
	interval time.Duration
	log      *slog.Logger
	// told is each conflict already reported, so that a pass every few seconds does not repeat one.
	told map[string]bool
}

// NewRecorder returns a Recorder over store and hosts. A zero interval is DefaultRecordInterval.
func NewRecorder(store Store, hosts PrefixHosts, clock Clock, interval time.Duration, log *slog.Logger) *Recorder {
	if interval <= 0 {
		interval = DefaultRecordInterval
	}
	return &Recorder{store: store, hosts: hosts, clock: clock, interval: interval, log: log, told: map[string]bool{}}
}

// Run records every interval until ctx ends, and returns ctx's error. A pass that fails is logged
// and does not end the loop.
func (r *Recorder) Run(ctx context.Context) error {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("prefix pass failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Reconcile is one pass. A prefix another host holds is an event and is never overwritten.
func (r *Recorder) Reconcile(ctx context.Context) error {
	reported, err := r.hosts.MachinePrefixes(ctx)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(reported))
	for id := range reported {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var errs []error
	for _, id := range ids {
		for _, p := range reported[id] {
			pf, err := netip.ParsePrefix(p)
			if err != nil || pf != pf.Masked() || !pf.Addr().Is4() {
				r.log.Warn("a host reports a Machine prefix that is not an IPv4 prefix", "host", id, "prefix", p)
				continue
			}
			err = r.store.ClaimPrefix(ctx, Allocation{Prefix: pf.String(), HostID: id})
			switch {
			case err == nil, errors.Is(err, ErrNotFound):
			case errors.Is(err, ErrAllocationTaken):
				r.conflict(ctx, id, pf.String())
			default:
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (r *Recorder) conflict(ctx context.Context, host, prefix string) {
	key := host + " " + prefix
	if r.told[key] {
		return
	}
	r.told[key] = true
	r.log.Warn("a host reports a Machine prefix another host holds", "host", host, "prefix", prefix)
	ev := orchapi.Event{Time: r.clock.Now(), Type: EventPrefixConflict, Subject: host, Detail: map[string]string{"prefix": prefix}}
	if err := r.store.AppendEvent(ctx, ev); err != nil {
		r.log.Error("the prefix conflict was not recorded", "err", err.Error())
	}
}
