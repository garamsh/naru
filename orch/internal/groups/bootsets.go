package groups

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"sync"
	"time"
)

// bootSetParallel is how many hosts receive a boot set at once: a set is hundreds of MiB.
const bootSetParallel = 2

// maxBootSetBackoff is the longest a host that keeps failing a boot set is left alone. The wait
// starts at one pass interval and doubles with each failure in a row.
const maxBootSetBackoff = 10 * time.Minute

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
// never removes a boot set. What it keeps between passes is the backoff of the hosts that fail.
type BootSetReconciler struct {
	hosts    BootSetHosts
	store    Store
	releases ReleaseStore
	clock    Clock
	events   Events
	interval time.Duration
	log      *slog.Logger

	state   sync.Mutex
	failing map[bootSetKey]backoff
}

// bootSetKey is one boot set a host lacks.
type bootSetKey struct{ host, name string }

// backoff is how many passes in a row failed for a bootSetKey, and when it is tried again.
type backoff struct {
	fails int
	next  time.Time
}

// NewBootSets returns a BootSetReconciler over the given adapters.
func NewBootSets(c BootSetConfig) *BootSetReconciler {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	return &BootSetReconciler{hosts: c.Hosts, store: c.Store, releases: c.Releases, clock: c.Clock, events: c.Events, interval: c.Interval, log: c.Log, failing: map[bootSetKey]backoff{}}
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
	needs := r.due(missingBootSets(specs, hosts), r.clock.Now())
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
				fail(r.failed(ctx, host, n, err, "release not fetched"))
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
		return r.failed(ctx, host, n, err, "host did not take the set")
	}
	return r.record(ctx, Event{Kind: EventBootSetAdded, Host: host, Message: fmt.Sprintf("boot set %s for node version %s", n.name, n.version)})
}

// failed records that n did not reach host and backs the host off. The event's class is the
// store's Failure class, or otherwise, so that the same refusal worded differently is one event.
func (r *BootSetReconciler) failed(ctx context.Context, host string, n bootSetNeed, err error, otherwise string) error {
	class := otherwise
	var f *Failure
	if errors.As(err, &f) {
		class = f.Class
	}
	now := r.clock.Now()
	r.state.Lock()
	b := r.failing[bootSetKey{host, n.name}]
	b.fails++
	delay := maxBootSetBackoff
	if b.fails <= 16 {
		delay = min(r.interval<<(b.fails-1), maxBootSetBackoff)
	}
	b.next = now.Add(delay)
	r.failing[bootSetKey{host, n.name}] = b
	r.state.Unlock()
	return r.record(ctx, Event{Kind: EventBootSetFailed, Host: host, Class: n.name + ": " + class,
		Message: fmt.Sprintf("boot set %s for node version %s: %v", n.name, n.version, err)})
}

// due drops from needs the hosts still backing off at now, and forgets the backoff of every set no
// host lacks any longer, so that the state is bounded by what the groups need.
func (r *BootSetReconciler) due(needs []bootSetNeed, now time.Time) []bootSetNeed {
	r.state.Lock()
	defer r.state.Unlock()
	lacking := map[bootSetKey]bool{}
	var out []bootSetNeed
	for _, n := range needs {
		var ready []string
		for _, h := range n.hosts {
			k := bootSetKey{h, n.name}
			lacking[k] = true
			if b, ok := r.failing[k]; !ok || !now.Before(b.next) {
				ready = append(ready, h)
			}
		}
		if len(ready) > 0 {
			n.hosts = ready
			out = append(out, n)
		}
	}
	maps.DeleteFunc(r.failing, func(k bootSetKey, _ backoff) bool { return !lacking[k] })
	return out
}

func (r *BootSetReconciler) record(ctx context.Context, e Event) error {
	e.Time = r.clock.Now()
	if err := r.events.Record(ctx, e); err != nil {
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
