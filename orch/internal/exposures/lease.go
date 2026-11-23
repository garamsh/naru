package exposures

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
)

// Lease timing (ADR 0085 item 5). A host removes an address LeaseSeconds after it received the
// last renewal; orch renews every pass, so the pass interval is a third of the lease.
const (
	LeaseSeconds    = 15
	DefaultInterval = LeaseSeconds * time.Second / 3
	// Skew is added to a lease's deadline before orch moves its address to another host that did
	// not confirm the release.
	Skew = 2 * time.Second
	// CallTimeout bounds every call of a pass to the hosts, and the write of the deadline before
	// a renewal. A host cannot receive a renewal later than CallTimeout after the deadline was
	// computed, so the deadline stored is never earlier than the end of the host's lease; and one
	// host that does not answer delays no other exposure's renewal for longer than this.
	CallTimeout = 2 * time.Second
	// SilentGrace is how long a host that was admitted but did not answer the listing keeps the
	// addresses it holds: they are renewed on it as before, so a host that is slow but healthy does
	// not lose them and take them back. Past it the host is taken for gone and they move after the
	// lease and Skew.
	SilentGrace = 2 * LeaseSeconds * time.Second
)

// Host is an admitted host the reconciler may lease an address to. LAN is the interface it
// announces on; a host with none is no candidate. Networks are the networks it runs a microVM on.
// Members maps each network to the addresses of its running microVMs, so the reconciler knows
// which of an exposure's backends run on a given network on this host; a host that runs none of
// an exposure's backends on the exposure's network is no candidate for it. Silent is an admitted
// host that did not answer this pass: it is no candidate for a new address, and keeps the ones
// it holds for SilentGrace.
type Host struct {
	ID       string
	LAN      string
	Networks []string
	Members  map[string][]string
	Silent   bool
}

// Lease is what a host is told to hold.
type Lease struct {
	Network       string
	Ports         []orchapi.ExposurePort
	Backends      []string
	ProxyProtocol string
	Seconds       int
}

// Hosts is the host API as the reconciler needs it.
type Hosts interface {
	// List returns the admitted hosts, with their LAN interface and networks; one that did not
	// answer is returned Silent.
	List(ctx context.Context) ([]Host, error)
	// Put takes the lease on the address on the host, or renews it.
	Put(ctx context.Context, hostID, address string, l Lease) error
	// Held returns the addresses the host holds a lease on.
	Held(ctx context.Context, hostID string) ([]string, error)
	// Delete removes the address from the host; an address it does not hold is not an error.
	Delete(ctx context.Context, hostID, address string) error
}

// Exposures is the store the reconciler reads and writes.
type Exposures interface {
	ListExposures(ctx context.Context) ([]Record, error)
	UpdateExposure(ctx context.Context, r, prev Record) error
	DeleteExposure(ctx context.Context, r Record) error
}

// Config is what NewReconciler needs. Whoever holds the leader lease constructs and runs one.
type Config struct {
	Hosts     Hosts
	Exposures Exposures
	Networks  Networks
	Events    Log
	Clock     Clock
	Interval  time.Duration
	Log       *slog.Logger
}

// Reconciler puts a lease on each exposure's address on one host and keeps it renewed.
type Reconciler struct {
	hosts    Hosts
	store    Exposures
	networks Networks
	events   Log
	clock    Clock
	interval time.Duration
	log      *slog.Logger

	// silentSince is when each host now silent was first found so, in this reconciler's memory: a
	// new leader starts the grace again.
	mu          sync.Mutex
	silentSince map[string]time.Time
	// refused holds the exposures this reconciler has already recorded a refusal of, by network and id,
	// so the event is written once and not on every pass; a new leader records it again.
	refused map[string]bool
}

// NewReconciler returns a Reconciler.
func NewReconciler(c Config) *Reconciler {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	return &Reconciler{hosts: c.Hosts, store: c.Exposures, networks: c.Networks, events: c.Events, clock: c.Clock, interval: c.Interval, log: c.Log, silentSince: map[string]time.Time{}, refused: map[string]bool{}}
}

// Run reconciles every interval until ctx ends, and returns ctx's error.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("exposure pass failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Choose returns the host that should hold the address among the candidates, by rendezvous
// hashing, so a lost host moves only its own addresses and a returned host takes back only its own.
// A candidate is a host that answered, with a LAN interface, that runs a microVM on the network,
// and that runs at least one of the exposure's backends on that network (ADR 0085 item 6, as
// amended by #296). Members is keyed by network so a backend address on one network cannot match
// a microVM of the same address on a different network.
func Choose(address, network string, backends []string, hosts []Host) (string, bool) {
	best, bestScore := "", uint64(0)
	for _, h := range hosts {
		if h.Silent || h.LAN == "" || !slices.Contains(h.Networks, network) {
			continue
		}
		if !slices.ContainsFunc(backends, func(b string) bool { return slices.Contains(h.Members[network], b) }) {
			continue
		}
		sum := sha256.Sum256([]byte(address + "\x00" + h.ID))
		if s := binary.BigEndian.Uint64(sum[:8]); best == "" || s > bestScore || s == bestScore && h.ID < best {
			best, bestScore = h.ID, s
		}
	}
	return best, best != ""
}

// localBackends returns the backends from the exposure that run on h's network, by intersection
// with h.Members[network].
func localBackends(h Host, network string, backends []string) []string {
	var out []string
	for _, b := range backends {
		if slices.Contains(h.Members[network], b) {
			out = append(out, b)
		}
	}
	return out
}

// Reconcile runs one pass. For each exposure it renews the lease on its host while that host is
// still the choice. An address moves only after the old host confirmed that it removed it, or
// after the old lease's deadline and Skew have passed, so at most one host holds an address except
// for a clock skew beyond Skew. The same rule frees the address of an exposure being released and
// of one whose network is gone. A lease on a host for an address no exposure holds is removed.
// Every call is bounded by CallTimeout and each exposure and host is served on its own, so one
// host that hangs delays no other renewal.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	lctx, cancel := context.WithTimeout(ctx, CallTimeout)
	hosts, err := r.hosts.List(lctx)
	cancel()
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}
	hosts = r.settle(hosts)
	// The exposures are read before the networks: a network made in between is seen, while an
	// exposure made in between waits for the next pass, so no exposure is taken for one whose
	// network is gone because its network was made after the networks were read.
	recs, err := r.store.ListExposures(ctx)
	if err != nil {
		return fmt.Errorf("list exposures: %w", err)
	}
	nets, err := r.networks.List(ctx)
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	live := map[string]bool{}
	for _, n := range nets {
		live[n.Name] = true
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	var errs []error
	var mu sync.Mutex
	fail := func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for _, rec := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.one(ctx, hosts, live[rec.Network], rec); err != nil {
				fail(fmt.Errorf("exposure %s: %w", rec.ID, err))
			}
		}()
	}
	wg.Wait()
	known := map[string]bool{}
	for _, rec := range recs {
		known[rec.Address] = true
	}
	for _, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hctx, cancel := context.WithTimeout(ctx, CallTimeout)
			defer cancel()
			held, err := r.hosts.Held(hctx, h.ID)
			if err != nil {
				r.log.Warn("host skipped: no leases", "host", h.ID, "err", err.Error())
				return
			}
			for _, a := range held {
				if known[a] {
					continue
				}
				if err := r.hosts.Delete(hctx, h.ID, a); err != nil {
					fail(fmt.Errorf("remove %s from host %s: %w", a, h.ID, err))
				}
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// settle drops the hosts that have been silent longer than SilentGrace, and says so, and forgets
// the hosts that answered again.
func (r *Reconciler) settle(hosts []Host) []Host {
	now := r.clock.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	since := map[string]time.Time{}
	out := hosts[:0:0]
	for _, h := range hosts {
		if !h.Silent {
			out = append(out, h)
			continue
		}
		t, ok := r.silentSince[h.ID]
		if !ok {
			t = now
			r.log.Warn("host did not answer; it keeps its addresses for now", "host", h.ID, "grace", SilentGrace.String())
		}
		since[h.ID] = t
		if now.Sub(t) > SilentGrace {
			r.log.Warn("host dropped: silent past the grace", "host", h.ID)
			continue
		}
		out = append(out, h)
	}
	r.silentSince = since
	return out
}

// silent reports whether the host is in the list and did not answer.
func silent(hosts []Host, id string) bool {
	return slices.ContainsFunc(hosts, func(h Host) bool { return h.ID == id && h.Silent })
}

// vacated reports whether the host that holds the exposure's address no longer does: it answered
// that it removed it, or its lease and Skew have passed.
func (r *Reconciler) vacated(ctx context.Context, hosts []Host, rec Record, now time.Time) bool {
	if rec.Host == "" {
		return true
	}
	if slices.ContainsFunc(hosts, func(h Host) bool { return h.ID == rec.Host }) && r.hosts.Delete(ctx, rec.Host, rec.Address) == nil {
		return true
	}
	return !now.Before(rec.LeaseUntil.Add(Skew))
}

// EventExposureRefused is the event of a stored exposure the reconciler does not lease because a port
// falls outside its owner's row of BackendPorts, or its owner has none: one stored before ADR 0102.
const EventExposureRefused = "exposure.refused"

// refuse returns the error that keeps rec from being leased, nil when it may be. It logs the refusal
// at error level on every pass and records the event once.
func (r *Reconciler) refuse(ctx context.Context, rec Record) error {
	var err error
	for _, p := range rec.Ports {
		if err = checkBackendPort(rec.Owner, p.BackendPort); err != nil {
			break
		}
	}
	if err == nil {
		return nil
	}
	reason := "stored before ADR 0102: delete it and let its owner re-apply (" + err.Error() + ")"
	r.log.Error("exposure not leased", "exposure", rec.ID, "network", rec.Network, "reason", reason)
	key := rec.Network + "/" + rec.ID
	r.mu.Lock()
	first := !r.refused[key]
	r.refused[key] = true
	r.mu.Unlock()
	if first {
		ev := orchapi.Event{Time: r.clock.Now(), Type: EventExposureRefused, Subject: rec.ID, Detail: map[string]string{"network": rec.Network, "address": rec.Address, "reason": reason}}
		if aerr := r.events.AppendEvent(ctx, ev); aerr != nil {
			r.log.Error("exposure refusal not recorded", "exposure", rec.ID, "err", aerr.Error())
		}
	}
	return err
}

func (r *Reconciler) one(ctx context.Context, hosts []Host, networkLive bool, rec Record) error {
	start := r.clock.Now()
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	if rec.Phase == PhaseReleasing || !networkLive {
		if !r.vacated(ctx, hosts, rec, start) {
			return nil // its lease may still run: wait for the confirmation or the deadline
		}
		return r.store.DeleteExposure(ctx, rec)
	}
	if err := r.refuse(ctx, rec); err != nil {
		return nil // nothing is leased for it, so no host is asked for what it will refuse on every pass
	}
	want, ok := Choose(rec.Address, rec.Network, rec.Backends, hosts)
	if rec.Host != "" && silent(hosts, rec.Host) {
		want, ok = rec.Host, true // slow, not known gone: renewed where it is, and not moved and moved back
	}
	next := rec
	if rec.Host != "" && rec.Host != want {
		if !r.vacated(ctx, hosts, rec, start) {
			return nil
		}
		next.Host, next.LeaseUntil, next.LeasedBackends, next.Phase = "", time.Time{}, nil, orchapi.ExposurePending
		if err := r.store.UpdateExposure(ctx, next, rec); err != nil {
			return err
		}
		rec = next
	}
	if !ok {
		if rec.Phase != orchapi.ExposurePending {
			next.Phase = orchapi.ExposurePending
			return r.store.UpdateExposure(ctx, next, rec)
		}
		return nil
	}
	// The lease carries only the backends that run on the announcing host on the exposure's
	// network. For a silent holder whose Members are unknown, the last-leased backends are used so
	// the host keeps receiving the same list it already accepted (ADR 0085 item 6, as amended by
	// #296). LeasedBackends is written with the deadline in one UpdateExposure.
	local := rec.LeasedBackends
	for _, h := range hosts {
		if h.ID == want && !h.Silent {
			local = localBackends(h, rec.Network, rec.Backends)
			break
		}
	}
	// The deadline is stored before the host is asked, and the store write and the renewal both
	// end within CallTimeout of start. The host receives the renewal by then and holds the address
	// LeaseSeconds after that, so this is never earlier than the end of its lease, however long
	// the renewal takes or whichever leader asks next.
	until := start.Add(CallTimeout + LeaseSeconds*time.Second)
	next.Host, next.LeaseUntil, next.LeasedBackends = want, until, local
	if err := r.store.UpdateExposure(ctx, next, rec); err != nil {
		return err
	}
	l := Lease{Network: rec.Network, Ports: rec.Ports, Backends: local, ProxyProtocol: rec.ProxyProtocol, Seconds: LeaseSeconds}
	if err := r.hosts.Put(ctx, want, rec.Address, l); err != nil {
		return fmt.Errorf("lease on host %s: %w", want, err)
	}
	if next.Phase != orchapi.ExposureAnnounced {
		prev := next
		next.Phase = orchapi.ExposureAnnounced
		return r.store.UpdateExposure(ctx, next, prev)
	}
	return nil
}
