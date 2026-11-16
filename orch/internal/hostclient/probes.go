package hostclient

import (
	"sync"

	"github.com/garamsh/naru/lib/orchapi"
)

// probeFailures is how many probes of one host in a row must fail before the network reconciler
// is told so. Each earlier failure is answered with the host's last answer: a single lost probe
// (a timeout while the host is busy) would otherwise remove the host's WireGuard peers and routes
// from every other host until the next pass. At the reconciler's 10 s pass, 3 is 30 s of silence.
const probeFailures = 3

// probes keeps, per host, the last answer of one probe and how many probes failed since.
type probes[T any] struct {
	mu sync.Mutex
	by map[string]*probed[T]
}

type probed[T any] struct {
	last   T
	has    bool
	failed int
}

// get runs fetch for the host. A failure is answered with the last answer until probeFailures
// probes in a row have failed, and returned after that, or when there is no last answer.
func (p *probes[T]) get(id string, fetch func() (T, error)) (T, error) {
	out, err := fetch()
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.by[id]
	if e == nil {
		e = &probed[T]{}
		p.by[id] = e
	}
	if err == nil {
		e.last, e.has, e.failed = out, true, 0
		return out, nil
	}
	e.failed++
	if e.has && e.failed < probeFailures {
		return e.last, nil
	}
	var zero T
	return zero, err
}

// keep forgets the hosts that are no longer admitted.
func (p *probes[T]) keep(admitted map[string]orchapi.Host) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id := range p.by {
		if _, ok := admitted[id]; !ok {
			delete(p.by, id)
		}
	}
}
