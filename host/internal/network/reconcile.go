package network

import (
	"context"
	"log/slog"
	"net/netip"
	"time"
)

// Reconciler keeps the members of a Manager to the machines of the host.
type Reconciler struct {
	Manager *Manager
	// Addresses lists the addresses of the host's machines.
	Addresses func(ctx context.Context) ([]netip.Addr, error)
	Interval  time.Duration
	Log       *slog.Logger
}

// Once makes one pass.
func (r *Reconciler) Once(ctx context.Context) error {
	return r.Manager.Reconcile(ctx, r.Addresses)
}

// Run makes a pass every Interval until ctx ends; a failed pass is logged and the next one tries again.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Once(ctx); err != nil && ctx.Err() == nil {
				r.Log.Error("cannot reconcile the networks' members with the machines", "err", err.Error())
			}
		}
	}
}
