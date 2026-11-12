// Package timesync keeps the Node's clock in step with the SNTP servers of spec.time
// (docs/architecture/adr/0021-init-keeps-time-from-spec-time-servers-over-sntp.md).
package timesync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	// BootBound is the longest the first synchronisation at boot takes, all servers together.
	BootBound = 10 * time.Second
	// ServerTimeout is how long one server is given, name resolution included.
	ServerTimeout = 3 * time.Second
	// PollInterval is the time between synchronisations once one has succeeded. It is longer than
	// the 256 s a slew of StepThreshold takes at the kernel's 500 ppm, so a slew has ended before the
	// next offset is measured.
	PollInterval = 300 * time.Second
	// RetryInterval is the time before the next attempt after one that no server answered.
	RetryInterval = 60 * time.Second
	// StepThreshold is the offset from which the clock is stepped and below which it is slewed.
	StepThreshold = 128 * time.Millisecond
)

// Correction is how a synchronisation moved the clock.
type Correction string

const (
	// Step set the clock to the server's time at once.
	Step Correction = "step"
	// Slew asked the kernel to bring the clock in line gradually.
	Slew Correction = "slew"
)

// Clock is the machine's clock, and what moves it.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
	// Step sets the clock offset ahead of what it reads now.
	Step(offset time.Duration) error
	// Slew has the kernel bring the clock in line by offset gradually.
	Slew(offset time.Duration) error
}

// Result is one successful synchronisation.
type Result struct {
	// Server is the host:port that answered.
	Server string
	// Offset is how far the server was ahead of the clock before the correction, and Delay the
	// round trip to it.
	Offset, Delay time.Duration
	Correction    Correction
	// At is when the correction was made, by the clock before it.
	At time.Time
}

// LogValue is the result as a log group.
func (r Result) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("server", r.Server),
		slog.String("offset", r.Offset.String()),
		slog.String("delay", r.Delay.String()),
		slog.String("correction", string(r.Correction)),
	)
}

// Status is the Syncer's state: the servers it asks, the last synchronisation and the last
// attempt that failed.
type Status struct {
	// Servers is empty when init keeps no time.
	Servers []string
	// Last is nil until a synchronisation has succeeded.
	Last *Result
	// Failed is when the latest attempt found no server, and Err why; both are zero when the
	// latest attempt succeeded.
	Failed time.Time
	Err    string
}

// Syncer asks the servers it is given and corrects the clock by what they say.
type Syncer struct {
	clock Clock
	query Querier

	syncing sync.Mutex // one synchronisation at a time

	mu      sync.Mutex
	servers []string
	gen     int           // counts Configure calls
	due     time.Time     // the next synchronisation; the zero time is at once
	status  Status        // Servers is left out; Status adds it
	wake    chan struct{} // Configure signals Run
}

// New returns a Syncer that asks through query and corrects clock. It asks no server until
// Configure gives it some.
func New(clock Clock, query Querier) *Syncer {
	return &Syncer{clock: clock, query: query, wake: make(chan struct{}, 1)}
}

// Configure replaces the servers with servers, each a host:port, and has Run synchronise at once.
// With none, the Syncer stops asking and leaves the clock where it is.
func (s *Syncer) Configure(servers []string) {
	s.mu.Lock()
	s.servers = append([]string(nil), servers...)
	s.gen++
	s.due = time.Time{}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Status returns the state now.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Servers = append([]string(nil), s.servers...)
	if st.Last != nil {
		last := *st.Last
		st.Last = &last
	}
	return st
}

// SyncNow asks the servers in order until one answers, and corrects the clock by its answer:
// a step when the offset is StepThreshold or more either way, a slew otherwise. It returns an
// error joining each server's failure when none answers, and gives up when ctx ends. Each server
// is given ServerTimeout.
func (s *Syncer) SyncNow(ctx context.Context) (Result, error) {
	s.syncing.Lock()
	defer s.syncing.Unlock()

	s.mu.Lock()
	servers, gen := append([]string(nil), s.servers...), s.gen
	s.mu.Unlock()

	res, err := s.sync(ctx, servers)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.status.Failed, s.status.Err = s.clock.Now(), err.Error()
	} else {
		s.status.Last, s.status.Failed, s.status.Err = &res, time.Time{}, ""
	}
	// A Configure during the attempt asked for another at once, which this one must not defer.
	if gen == s.gen {
		interval := PollInterval
		if err != nil {
			interval = RetryInterval
		}
		s.due = s.clock.Now().Add(interval)
	}
	return res, err
}

func (s *Syncer) sync(ctx context.Context, servers []string) (Result, error) {
	if len(servers) == 0 {
		return Result{}, errors.New("no time server is configured")
	}
	var errs []error
	for _, addr := range servers {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		attempt, cancel := context.WithTimeout(ctx, ServerTimeout)
		smp, err := s.query.Query(attempt, addr)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", addr, err))
			continue
		}
		res := Result{Server: addr, Offset: smp.Offset, Delay: smp.Delay, At: s.clock.Now()}
		if smp.Offset >= StepThreshold || smp.Offset <= -StepThreshold {
			res.Correction, err = Step, s.clock.Step(smp.Offset)
		} else {
			res.Correction, err = Slew, s.clock.Slew(smp.Offset)
		}
		if err != nil {
			return Result{}, fmt.Errorf("%s: %s the clock: %w", addr, res.Correction, err)
		}
		return res, nil
	}
	return Result{}, errors.Join(errs...)
}

// Run synchronises when Configure asks and every PollInterval after a success, or RetryInterval
// after a failure, until ctx ends, and logs each outcome. It waits while no server is configured.
func (s *Syncer) Run(ctx context.Context, log *slog.Logger) {
	for ctx.Err() == nil {
		// A signal sent before the state is read below is already in it.
		select {
		case <-s.wake:
		default:
		}
		s.mu.Lock()
		idle, due := len(s.servers) == 0, s.due
		s.mu.Unlock()
		var timer <-chan time.Time
		if !idle {
			wait := due.Sub(s.clock.Now())
			if wait <= 0 {
				res, err := s.SyncNow(ctx)
				switch {
				case ctx.Err() != nil:
				case err != nil:
					log.Warn("time not synchronised; will try again", "err", err, "retry", RetryInterval.String())
				default:
					log.Info("time synchronised", "sync", res)
				}
				continue
			}
			timer = s.clock.After(wait)
		}
		select {
		case <-ctx.Done():
		case <-s.wake:
		case <-timer:
		}
	}
}
