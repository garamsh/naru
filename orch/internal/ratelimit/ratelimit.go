// Package ratelimit is the token bucket per name that bounds what one source may ask of orch.
package ratelimit

import (
	"sync"
	"time"
)

// Clock is the time source of a Limiter.
type Clock interface {
	Now() time.Time
}

// maxBuckets is how many idle buckets a limiter keeps before it drops the ones that refilled.
const maxBuckets = 4096

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a token bucket per name: burst tokens at most, refilled at rate per second.
type Limiter struct {
	mu      sync.Mutex
	clock   Clock
	rate    float64
	burst   float64
	buckets map[string]*bucket
}

// New returns a Limiter of burst tokens per name, refilled at rate per second.
func New(clock Clock, rate float64, burst int) *Limiter {
	return &Limiter{clock: clock, rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

// Allow takes a token of the name's bucket and reports whether there was one.
func (l *Limiter) Allow(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	if len(l.buckets) >= maxBuckets {
		for n, b := range l.buckets {
			if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
				delete(l.buckets, n)
			}
		}
	}
	b, ok := l.buckets[name]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[name] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
