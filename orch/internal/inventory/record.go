package inventory

import (
	"context"
	"errors"
	"fmt"
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
// a configuration of its own (it did), and gives a mesh address to every admitted host that has none,
// such as one admitted before the profile had a mesh pool. Whoever holds the leader lease runs one.
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

// Reconcile is one pass. A prefix another host holds is an event and is never overwritten, a
// confirmed allocation of a host that is no longer admitted is released, and a host with no mesh
// address is given one.
func (r *Recorder) Reconcile(ctx context.Context) error {
	if err := r.releaseOrphans(ctx); err != nil {
		return err
	}
	if err := r.giveMeshAddresses(ctx); err != nil {
		return err
	}
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

// releaseOrphans releases every confirmed allocation whose host is not admitted: a claim that
// committed just before its host was removed, which the removal did not see. The allocations are
// read before the hosts, and a confirmed one is written in the transaction that writes its host, so
// a host missing from the second read was removed after the first; the release deletes the record
// only while it still holds the value read.
func (r *Recorder) releaseOrphans(ctx context.Context) error {
	allocations, err := r.store.ListAllocations(ctx, SpaceMachine)
	if err != nil {
		return err
	}
	mesh, err := r.store.ListAllocations(ctx, SpaceMesh)
	if err != nil {
		return err
	}
	allocations = append(allocations, mesh...)
	hosts, err := r.store.ListHosts(ctx)
	if err != nil {
		return err
	}
	admitted := map[string]bool{}
	for _, h := range hosts {
		admitted[h.ID] = true
	}
	var errs []error
	for _, a := range allocations {
		if a.ReservedUntil.IsZero() && !admitted[a.HostID] {
			r.log.Warn("releasing an allocation of a host that is not admitted", "host", a.HostID, "prefix", a.Prefix)
			errs = append(errs, r.store.Release(ctx, a))
		}
	}
	return errors.Join(errs...)
}

// giveMeshAddresses gives each admitted host that has no mesh address one from the profile's mesh
// pool, when it has one.
func (r *Recorder) giveMeshAddresses(ctx context.Context) error {
	pool, err := MeshPool(ctx, r.store)
	if err != nil || !pool.IsValid() {
		return err
	}
	hosts, err := r.store.ListHosts(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, h := range hosts {
		if h.MeshAddress != "" {
			continue
		}
		got, err := r.store.GiveMeshAddress(ctx, h.ID, meshPickFrom(ctx, r.store))
		switch {
		case err == nil:
			r.log.Info("gave a host its mesh address", "host", h.ID, "mesh_address", got.MeshAddress)
		case errors.Is(err, ErrNotFound):
		default:
			errs = append(errs, fmt.Errorf("host %s: mesh address: %w", h.ID, err))
		}
	}
	return errors.Join(errs...)
}
