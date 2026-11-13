// Package fence keeps mvmd's fencing deadline armed. naru-hostd is the one client that renews it
// (docs/architecture/host.md): mvmd refuses to start a Machine while the deadline is not armed, and
// stops every Machine by force when it passes, so a host whose hostd has stopped renewing stops its
// Machines within Length.
package fence

import (
	"context"
	"log/slog"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
)

// Renewer renews the deadline every Interval for Length.
type Renewer struct {
	Runtime  mvmd.Runtime
	Length   time.Duration
	Interval time.Duration
	// Retry is how long it waits after a failed attempt, when mvmd is down or restarting.
	Retry time.Duration
	Log   *slog.Logger
}

// Run renews until ctx ends. Each renewal carries a number above the last one mvmd applied, read
// from mvmd before the first call and again whenever a renewal fails, so a restart of hostd, or of
// mvmd with its counter intact, continues above it. A failed attempt is logged once per failure
// streak, and retried every Retry until mvmd answers.
func (r *Renewer) Run(ctx context.Context) {
	var number uint64
	known, failing := false, false
	for {
		wait := r.Interval
		if err := r.once(ctx, &number, &known); err != nil {
			if ctx.Err() != nil {
				return
			}
			known, wait = false, r.Retry
			if !failing {
				r.Log.Error("cannot renew mvmd's fencing deadline; retrying", "err", err.Error(), "retry", r.Retry.String())
			}
			failing = true
		} else {
			if failing {
				r.Log.Info("renewed mvmd's fencing deadline again", "renewal", number)
			}
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (r *Renewer) once(ctx context.Context, number *uint64, known *bool) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !*known {
		d, err := r.Runtime.InspectDeadline(cctx)
		if err != nil {
			return err
		}
		*number, *known = d.LastNumber, true
	}
	*number++
	return r.Runtime.Renew(cctx, r.Length, *number)
}
