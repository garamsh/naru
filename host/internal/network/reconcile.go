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
	// Timeout bounds a pass, which holds the Manager's lock while it lists the machines.
	Timeout time.Duration
	Log     *slog.Logger

	adopted bool
}

// Once makes one pass, which ends with an error after Timeout. The first pass that succeeds adopts
// as Machines the microVMs on no network (Manager.Adopt); it is made before the API serves.
func (r *Reconciler) Once(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	if r.adopted {
		return r.Manager.Reconcile(ctx, r.Addresses)
	}
	if err := r.Manager.Adopt(ctx, r.Addresses); err != nil {
		return err
	}
	r.adopted = true
	return nil
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
