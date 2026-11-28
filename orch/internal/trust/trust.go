// Package trust warns of a pool of authorities that has not taken an authority's renewed
// certificate a year before the pool stops trusting it (ADR 0121).
//
// The signer signs each authority again under its key when two of its ten years remain, and every
// pool that trusts the authority takes the new certificate within those two years. A pool that
// missed it is seen here: orch reads its own pools and the expiry each admitted host reports for
// its own, and logs and appends authority.expiring for each that ends within a year.
package trust

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// EventExpiring is a pool that stops trusting an authority it holds within
// identity.AuthorityWarnYears: its subject is the orch member or host that holds the pool, and its
// detail the pool, the authority and when the pool stops trusting it.
const EventExpiring = "authority.expiring"

// DefaultInterval is how often a Watcher checks when its Config names none.
const DefaultInterval = time.Hour

// Config is what a Watcher checks and where it warns.
type Config struct {
	// Member is the name of the orch member whose Pools these are.
	Member string
	// Pools are this member's pools by the flag that names them, each the files of PEM
	// certificates read together: the first, which must be there, and the stores of re-signings
	// after it, which may be absent (ADR 0123).
	Pools map[string][]string
	// Hosts, when set, are the admitted hosts whose reported pools are checked too: the leader's.
	Hosts    Hosts
	Events   Events
	Clock    Clock
	Interval time.Duration
	Log      *slog.Logger
}

// Watcher checks its pools every interval.
type Watcher struct {
	cfg Config

	mu sync.Mutex
	// warned are the warnings already given by this Watcher, by subject, pool and end.
	warned map[string]bool
}

// New returns the Watcher of cfg.
func New(cfg Config) *Watcher {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Watcher{cfg: cfg, warned: map[string]bool{}}
}

// Run checks now and every interval until ctx ends.
func (w *Watcher) Run(ctx context.Context) error {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		if err := w.Check(ctx); err != nil && ctx.Err() == nil {
			w.cfg.Log.Error("cannot check the pools of authorities", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Check reads every pool once and warns of each that ends within identity.AuthorityWarnYears, once
// per subject, pool, authority and end. A pool that cannot be read is an error, and the others are checked.
func (w *Watcher) Check(ctx context.Context) error {
	now := w.cfg.Clock.Now()
	var errs []error
	for name, files := range w.cfg.Pools {
		if len(files) == 0 {
			continue
		}
		b, err := identity.FilesPEM(files[0], files[1:]...)
		file := strings.Join(files, ",")
		if err != nil {
			errs = append(errs, fmt.Errorf("read the pool %s: %w", name, err))
			continue
		}
		if end, ok := identity.TrustExpiry(b); ok && identity.TrustWarns(end.At, now) {
			w.warn(ctx, "orch/"+w.cfg.Member, name, file, end)
		}
	}
	if w.cfg.Hosts != nil {
		hosts, err := w.cfg.Hosts.Trust(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("read the hosts' pools: %w", err))
		}
		for _, h := range hosts {
			if identity.TrustWarns(h.ExpiresAt, now) {
				w.warn(ctx, h.ID, "client-ca", "", identity.TrustEnd{At: h.ExpiresAt, Authority: h.Authority})
			}
		}
	}
	return errors.Join(errs...)
}

func (w *Watcher) warn(ctx context.Context, subject, pool, file string, end identity.TrustEnd) {
	at := end.At.UTC().Format(time.RFC3339)
	key := subject + "\x00" + pool + "\x00" + end.Authority + "\x00" + at
	w.mu.Lock()
	seen := w.warned[key]
	w.warned[key] = true
	w.mu.Unlock()
	if seen {
		return
	}
	w.cfg.Log.Warn("a pool stops trusting an authority within a year",
		"subject", subject, "pool", pool, "file", file, "authority", end.Authority, "expires_at", at)
	if w.cfg.Events == nil {
		return
	}
	detail := map[string]string{"pool": pool, "authority": end.Authority, "expiresAt": at}
	if file != "" {
		detail["file"] = file
	}
	ev := orchapi.Event{Time: w.cfg.Clock.Now().UTC(), Type: EventExpiring, Subject: subject, Detail: detail}
	if err := w.cfg.Events.AppendEvent(ctx, ev); err != nil {
		w.cfg.Log.Error("cannot append the event", "type", EventExpiring, "subject", subject, "err", err.Error())
		w.mu.Lock()
		delete(w.warned, key)
		w.mu.Unlock()
	}
}
