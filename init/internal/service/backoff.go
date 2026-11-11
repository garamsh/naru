package service

import "time"

// The restart delay is fixed in P0 (docs/architecture/config.md §Current decisions).
const (
	backoffInitial = time.Second
	backoffMax     = 30 * time.Second
	// backoffReset is how long a run must last for the next failure to count as the first.
	backoffReset = 60 * time.Second
)

// Backoff is the delay before a service that exited is started again: 1 s,
// doubled on each consecutive failure up to 30 s, and back to 1 s after a run
// of at least 60 s. The zero value is ready.
type Backoff struct {
	delay time.Duration
}

// Next returns the delay after a run that lasted ranFor.
func (b *Backoff) Next(ranFor time.Duration) time.Duration {
	if b.delay == 0 || ranFor >= backoffReset {
		b.delay = backoffInitial
	} else {
		b.delay = min(b.delay*2, backoffMax)
	}
	return b.delay
}
