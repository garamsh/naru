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

// Lease timing (ADR 0085 item 5). A host removes an address LeaseSeconds after its last renewal;
// orch renews every pass, so the pass interval is a third of the lease.
const (
	LeaseSeconds    = 15
	DefaultInterval = LeaseSeconds * time.Second / 3
	// Skew is added to a lease's deadline before orch moves its address to another host that did
	// not confirm the release.
	Skew = 2 * time.Second
)

// Host is an admitted host the reconciler may lease an address to. LAN is the interface it
// announces on; a host with none is no candidate. Networks are the networks it runs a microVM on.
type Host struct {
	ID       string
	LAN      string
	Networks []string
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
	// List returns the hosts that answered, with their LAN interface and networks.
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
}

// Config is what NewReconciler needs. Whoever holds the leader lease constructs and runs one.
type Config struct {
	Hosts     Hosts
	Exposures Exposures
	Clock     Clock
	Interval  time.Duration
	Log       *slog.Logger
}

// Reconciler puts a lease on each exposure's address on one host and keeps it renewed.
type Reconciler struct {
	hosts    Hosts
	store    Exposures
	clock    Clock
	interval time.Duration
	log      *slog.Logger
}

// NewReconciler returns a Reconciler.
func NewReconciler(c Config) *Reconciler {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	return &Reconciler{hosts: c.Hosts, store: c.Exposures, clock: c.Clock, interval: c.Interval, log: c.Log}
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
// A candidate is a host with a LAN interface that runs a microVM on the network.
func Choose(address, network string, hosts []Host) (string, bool) {
	best, bestScore := "", uint64(0)
	for _, h := range hosts {
		if h.LAN == "" || !slices.Contains(h.Networks, network) {
			continue
		}
		sum := sha256.Sum256([]byte(address + "\x00" + h.ID))
		if s := binary.BigEndian.Uint64(sum[:8]); best == "" || s > bestScore || s == bestScore && h.ID < best {
			best, bestScore = h.ID, s
		}
	}
	return best, best != ""
}

// Reconcile runs one pass. For each exposure it renews the lease on its host while that host is
// still the choice. An address moves only after the old host confirmed that it removed it, or
// after the old lease's deadline and Skew have passed, so at most one host holds an address except
// for a clock skew beyond Skew. A lease on a host for an address no exposure holds is removed.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	hosts, err := r.hosts.List(ctx)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}
	recs, err := r.store.ListExposures(ctx)
	if err != nil {
		return fmt.Errorf("list exposures: %w", err)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].ID < hosts[j].ID })
	var errs []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, rec := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.one(ctx, hosts, rec); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("exposure %s: %w", rec.ID, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	known := map[string]string{}
	for _, rec := range recs {
		known[rec.Address] = rec.ID
	}
	for _, h := range hosts {
		held, err := r.hosts.Held(ctx, h.ID)
		if err != nil {
			r.log.Warn("host skipped: no leases", "host", h.ID, "err", err.Error())
			continue
		}
		for _, a := range held {
			if _, ok := known[a]; ok {
				continue
			}
			if err := r.hosts.Delete(ctx, h.ID, a); err != nil {
				errs = append(errs, fmt.Errorf("remove %s from host %s: %w", a, h.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (r *Reconciler) one(ctx context.Context, hosts []Host, rec Record) error {
	now := r.clock.Now()
	want, ok := Choose(rec.Address, rec.Network, hosts)
	answers := func(id string) bool {
		return slices.ContainsFunc(hosts, func(h Host) bool { return h.ID == id })
	}
	next := rec
	if rec.Host != "" && rec.Host != want {
		// The holder is no longer the choice, or there is none: release it first.
		released := false
		if answers(rec.Host) {
			released = r.hosts.Delete(ctx, rec.Host, rec.Address) == nil
		}
		if !released && now.Before(rec.LeaseUntil.Add(Skew)) {
			return nil // its lease may still run: wait for the confirmation or the deadline
		}
		next.Host, next.LeaseUntil, next.Phase = "", time.Time{}, orchapi.ExposurePending
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
	// The deadline is stored before the host is asked, so a crash between the two leaves a record
	// that waits at least as long as the lease the host may have taken.
	until := now.Add(LeaseSeconds * time.Second)
	next.Host, next.LeaseUntil = want, until
	if !next.Equal(rec) {
		if err := r.store.UpdateExposure(ctx, next, rec); err != nil {
			return err
		}
	}
	l := Lease{Network: rec.Network, Ports: rec.Ports, Backends: rec.Backends, ProxyProtocol: rec.ProxyProtocol, Seconds: LeaseSeconds}
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
