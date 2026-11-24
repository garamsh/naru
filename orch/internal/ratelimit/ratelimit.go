// Package ratelimit is the token bucket per name that bounds what one source may ask of orch.
package ratelimit

import (
	"net"
	"net/netip"
	"sync"
	"time"
)

// Clock is the time source of a Limiter.
type Clock interface {
	Now() time.Time
}

// maxBuckets is how many buckets a limiter holds. At it, the limiter drops the buckets that have
// refilled, and when none has it refuses a name it holds no bucket for: a flood of new names is
// refused rather than let through or let grow the map.
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
	b, ok := l.buckets[name]
	if !ok && len(l.buckets) >= maxBuckets {
		for n, b := range l.buckets {
			if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
				delete(l.buckets, n)
			}
		}
		if len(l.buckets) >= maxBuckets {
			return false
		}
	}
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

// Source is the name a request's remote address is limited by: the IPv4 address, or the /64 an
// IPv6 address is in, since one IPv6 host holds every address of its /64. An address that does not
// parse is its own name.
func Source(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	a = a.Unmap().WithZone("")
	if a.Is4() {
		return a.String()
	}
	return netip.PrefixFrom(a, 64).Masked().String()
}
