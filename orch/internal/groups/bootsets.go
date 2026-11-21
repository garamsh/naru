package groups

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"
)

// bootSetParallel is how many hosts receive a boot set at once: a set is hundreds of MiB.
const bootSetParallel = 2

// BootSetConfig is what NewBootSets needs. Whoever holds the leader lease constructs and runs
// one BootSetReconciler.
type BootSetConfig struct {
	Hosts    BootSetHosts
	Store    Store
	Releases ReleaseStore
	Clock    Clock
	Events   Events
	Interval time.Duration
	Log      *slog.Logger
}

// BootSetReconciler adds to each host the boot sets the groups that can place on it need. It
// keeps no state between passes and never removes a boot set.
type BootSetReconciler struct {
	hosts    BootSetHosts
	store    Store
	releases ReleaseStore
	clock    Clock
	events   Events
	interval time.Duration
	log      *slog.Logger
}

// NewBootSets returns a BootSetReconciler over the given adapters.
func NewBootSets(c BootSetConfig) *BootSetReconciler {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	return &BootSetReconciler{hosts: c.Hosts, store: c.Store, releases: c.Releases, clock: c.Clock, events: c.Events, interval: c.Interval, log: c.Log}
}

// Run reconciles every interval until ctx ends and returns ctx's error. A failed pass is logged
// and does not end the loop.
func (r *BootSetReconciler) Run(ctx context.Context) error {
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("boot set pass failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.clock.After(r.interval):
		}
	}
}

// bootSetNeed is one boot set a host lacks, by the node version that names it.
type bootSetNeed struct {
	version string
	name    string
	hosts   []string
}

// Reconcile runs one pass: it finds the hosts that lack a boot set a group can use there, and for
// each node version verifies the release once and adds the set to those hosts, two at a time.
func (r *BootSetReconciler) Reconcile(ctx context.Context) error {
	specs, err := r.store.ListGroups(ctx)
	if err != nil {
		return fmt.Errorf("list groups: %w", err)
	}
	hosts, err := r.hosts.List(ctx)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}
	needs := missingBootSets(specs, hosts)
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
		slot = make(chan struct{}, bootSetParallel)
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}
	for _, n := range needs {
		set, err := r.releases.Fetch(ctx, n.version)
		if err != nil {
			for _, host := range n.hosts {
				fail(r.record(ctx, EventBootSetFailed, host, fmt.Sprintf("boot set %s for node version %s: %v", n.name, n.version, err)))
			}
			continue
		}
		for _, host := range n.hosts {
			slot <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slot }()
				if err := r.add(ctx, host, n, set); err != nil {
					fail(err)
				}
			}()
		}
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (r *BootSetReconciler) add(ctx context.Context, host string, n bootSetNeed, set BootSet) error {
	if err := r.hosts.AddBootSet(ctx, host, set); err != nil {
		return r.record(ctx, EventBootSetFailed, host, fmt.Sprintf("boot set %s for node version %s: %v", n.name, n.version, err))
	}
	return r.record(ctx, EventBootSetAdded, host, fmt.Sprintf("boot set %s for node version %s", n.name, n.version))
}

func (r *BootSetReconciler) record(ctx context.Context, kind EventKind, host, msg string) error {
	if err := r.events.Record(ctx, Event{Time: r.clock.Now(), Kind: kind, Host: host, Message: msg}); err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

// missingBootSets lists, by node version, the hosts that pass a group's placement filters but for
// the node version and do not offer the version's boot set. A group of the image form, or naming no
// version, or naming one that makes no boot set name, needs none.
func missingBootSets(specs []GroupSpec, hosts []Host) []bootSetNeed {
	byVersion := map[string]*bootSetNeed{}
	for _, spec := range specs {
		t := spec.Template
		if t.Form != FormOS || t.NodeVersion == "" {
			continue
		}
		for _, h := range hosts {
			name, offers := ResolveBootSet(t.NodeVersion, h.NodeVersions)
			if offers || name == "" || len(hostGaps(spec, h)) > 0 {
				continue
			}
			n, ok := byVersion[t.NodeVersion]
			if !ok {
				n = &bootSetNeed{version: t.NodeVersion, name: name}
				byVersion[t.NodeVersion] = n
			}
			if !slices.Contains(n.hosts, h.ID) {
				n.hosts = append(n.hosts, h.ID)
			}
		}
	}
	out := make([]bootSetNeed, 0, len(byVersion))
	for _, n := range byVersion {
		sort.Strings(n.hosts)
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out
}
