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
// a configuration of its own (it did), and gives a mesh address to every admitted host that has none,
// such as one admitted before the profile had a mesh pool. Whoever holds the leader lease runs one.
type Recorder struct {
	store    Store
	hosts    PrefixHosts
	clock    Clock
	interval time.Duration
	log      *slog.Logger
	// told is each conflict, and each host without a mesh address, already reported, so that a pass
	// every few seconds does not repeat one.
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

// Reconcile is one pass of three steps, each run whatever the others met. A confirmed allocation of
// a host that is no longer admitted is released; every host's reported Machine prefixes are
// recorded, and a prefix another host holds is an event and is never overwritten; a host with no
// mesh address is given one, and one it cannot be given is an event, which holds up no other host.
func (r *Recorder) Reconcile(ctx context.Context) error {
	return errors.Join(r.releaseOrphans(ctx), r.recordPrefixes(ctx), r.giveMeshAddresses(ctx))
}

// recordPrefixes claims every Machine prefix the hosts report as a confirmed allocation of its host.
func (r *Recorder) recordPrefixes(ctx context.Context) error {
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
	r.tell(ctx, host+" "+prefix, "a host reports a Machine prefix another host holds",
		orchapi.Event{Type: EventPrefixConflict, Subject: host, Detail: map[string]string{"prefix": prefix}})
}

// tell logs and records ev the first time key is met, so that a pass every few seconds repeats
// neither.
func (r *Recorder) tell(ctx context.Context, key, msg string, ev orchapi.Event) {
	if r.told[key] {
		return
	}
	r.told[key] = true
	args := []any{"host", ev.Subject}
	for k, v := range ev.Detail {
		args = append(args, k, v)
	}
	r.log.Warn(msg, args...)
	ev.Time = r.clock.Now()
	if err := r.store.AppendEvent(ctx, ev); err != nil {
		r.log.Error("the event was not recorded", "type", ev.Type, "err", err.Error())
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
	for _, h := range hosts {
		if h.MeshAddress != "" {
			continue
		}
		got, err := r.store.GiveMeshAddress(ctx, h.ID, meshPickFrom(ctx, r.store))
		switch {
		case err == nil:
			delete(r.told, "mesh "+h.ID)
			r.log.Info("gave a host its mesh address", "host", h.ID, "mesh_address", got.MeshAddress)
		case errors.Is(err, ErrNotFound), ctx.Err() != nil:
		default:
			r.tell(ctx, "mesh "+h.ID, "a host could not be given a mesh address",
				orchapi.Event{Type: EventMeshAddressUnavailable, Subject: h.ID, Detail: map[string]string{"reason": err.Error()}})
		}
	}
	return nil
}
