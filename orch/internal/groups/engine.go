package groups

import (
	"context"
	"fmt"
	"time"
)

// DefaultInterval is the pause between passes when Config.Interval is zero.
const DefaultInterval = 10 * time.Second

// Config is what New needs. Whoever holds the leader lease constructs and runs one Engine.
type Config struct {
	Hosts    Hosts
	Store    Store
	Clock    Clock
	Events   Events
	Interval time.Duration
}

// Engine runs the passes. It keeps no state between them: the next plan depends on what
// the next observation shows.
type Engine struct {
	hosts    Hosts
	store    Store
	clock    Clock
	events   Events
	interval time.Duration
}

// New returns an Engine over the given adapters.
func New(c Config) *Engine {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	return &Engine{hosts: c.Hosts, store: c.Store, clock: c.Clock, events: c.Events, interval: c.Interval}
}

// Observe reads the groups, the members and the hosts, and the fence of each stateful member
// whose host answers.
func (e *Engine) Observe(ctx context.Context) (Observed, error) {
	o := Observed{Now: e.clock.Now(), Fences: map[string]Fence{}}
	var err error
	if o.Groups, err = e.store.ListGroups(ctx); err != nil {
		return Observed{}, fmt.Errorf("list groups: %w", err)
	}
	if o.Members, err = e.store.ListMembers(ctx); err != nil {
		return Observed{}, fmt.Errorf("list members: %w", err)
	}
	if o.Hosts, err = e.hosts.List(ctx); err != nil {
		return Observed{}, fmt.Errorf("list hosts: %w", err)
	}
	stateful := map[string]bool{}
	for _, g := range o.Groups {
		stateful[g.Name] = g.Stateful
	}
	listed := map[string]bool{}
	for _, h := range o.Hosts {
		listed[h.ID] = true
	}
	for _, m := range o.Members {
		if !stateful[m.Group] || m.Phase == PhaseGone || !listed[m.Host] {
			continue
		}
		deadline, lapsed, err := e.hosts.InspectFence(ctx, m.Host, m.MachineID)
		if err != nil {
			ev := Event{Time: o.Now, Kind: EventObserveFail, Group: m.Group, Member: m.ID, Host: m.Host, Message: "inspect fence: " + err.Error()}
			if rerr := e.events.Record(ctx, ev); rerr != nil {
				return Observed{}, fmt.Errorf("record event: %w", rerr)
			}
			continue
		}
		o.Fences[m.ID] = Fence{Deadline: deadline, LapsedAt: lapsed}
	}
	return o, nil
}

// Reconcile runs one pass: observe, evaluate, plan, apply.
func (e *Engine) Reconcile(ctx context.Context) error {
	o, err := e.Observe(ctx)
	if err != nil {
		return err
	}
	return e.Apply(ctx, Plan(Evaluate(o), o))
}

// Run reconciles every interval until ctx ends. A failed pass is recorded and does not end
// the loop; Run returns ctx's error, or the error of an event that could not be recorded.
func (e *Engine) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := e.Reconcile(ctx); err != nil && ctx.Err() == nil {
			ev := Event{Time: e.clock.Now(), Kind: EventPassFailed, Message: err.Error()}
			if rerr := e.events.Record(ctx, ev); rerr != nil {
				return fmt.Errorf("record event: %w", rerr)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.clock.After(e.interval):
		}
	}
}
