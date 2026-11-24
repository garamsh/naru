// Package exposurestest holds in-memory fakes of the exposures service's adapters, for the tests
// of the packages that use it.
package exposurestest

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// Store is an exposures.Store in memory, with the claim, count and pool rules of the etcd one.
type Store struct {
	mu     sync.Mutex
	pools  map[string]orchapi.AddressPool
	exps   map[string]exposures.Record // by network/id
	claims map[string]exposures.Claim  // by pool/address
}

var _ exposures.Store = (*Store)(nil)

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{pools: map[string]orchapi.AddressPool{}, exps: map[string]exposures.Record{}, claims: map[string]exposures.Claim{}}
}

func recKey(network, id string) string { return network + "/" + id }

func (s *Store) PutPool(_ context.Context, p orchapi.AddressPool, prev *orchapi.AddressPool, check func([]exposures.Claim) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.pools[p.Name]
	if ok != (prev != nil) || ok && !reflect.DeepEqual(cur, *prev) {
		return inventory.ErrConflict
	}
	var held []exposures.Claim
	for k, c := range s.claims {
		if strings.HasPrefix(k, p.Name+"/") {
			held = append(held, c)
		}
	}
	// check reads the store, so it runs without the lock; the write holds only while the pool and
	// its claims are as they were.
	s.mu.Unlock()
	err := check(held)
	s.mu.Lock()
	if err != nil {
		return err
	}
	again, ok2 := s.pools[p.Name]
	n := 0
	for k := range s.claims {
		if strings.HasPrefix(k, p.Name+"/") {
			n++
		}
	}
	if ok2 != ok || ok && !reflect.DeepEqual(again, cur) || n != len(held) {
		return inventory.ErrConflict
	}
	s.pools[p.Name] = p
	return nil
}

func (s *Store) GetPool(_ context.Context, name string) (orchapi.AddressPool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pools[name]
	if !ok {
		return p, inventory.ErrNotFound
	}
	return p, nil
}

func (s *Store) DeletePool(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pools[name]; !ok {
		return inventory.ErrNotFound
	}
	for k := range s.claims {
		if strings.HasPrefix(k, name+"/") {
			return inventory.ErrConflict
		}
	}
	delete(s.pools, name)
	return nil
}

func (s *Store) ListPools(context.Context) ([]orchapi.AddressPool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []orchapi.AddressPool{}
	for _, p := range s.pools {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) CreateExposure(_ context.Context, r exposures.Record, pool orchapi.AddressPool, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	claim := r.Pool + "/" + r.Address
	n := 0
	for _, o := range s.exps {
		if o.Network == r.Network {
			n++
		}
	}
	if n >= limit {
		return exposures.ErrFull
	}
	if _, ok := s.exps[recKey(r.Network, r.ID)]; ok {
		return inventory.ErrConflict
	}
	if _, ok := s.claims[claim]; ok {
		return inventory.ErrConflict
	}
	if cur, ok := s.pools[pool.Name]; !ok || !reflect.DeepEqual(cur, pool) {
		return inventory.ErrConflict
	}
	s.exps[recKey(r.Network, r.ID)] = r
	s.claims[claim] = exposures.Claim{Address: r.Address, Network: r.Network, ID: r.ID}
	return nil
}

func (s *Store) UpdateExposure(_ context.Context, r, prev exposures.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.exps[recKey(r.Network, r.ID)]; !ok || !cur.Equal(prev) {
		return inventory.ErrConflict
	}
	s.exps[recKey(r.Network, r.ID)] = r
	return nil
}

func (s *Store) GetExposure(_ context.Context, network, id string) (exposures.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.exps[recKey(network, id)]
	if !ok {
		return r, inventory.ErrNotFound
	}
	return r, nil
}

func (s *Store) DeleteExposure(_ context.Context, r exposures.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.exps[recKey(r.Network, r.ID)]
	if !ok {
		return nil
	}
	if !cur.Equal(r) {
		return inventory.ErrConflict
	}
	delete(s.claims, r.Pool+"/"+r.Address)
	delete(s.exps, recKey(r.Network, r.ID))
	return nil
}

func (s *Store) ListExposures(context.Context) ([]exposures.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []exposures.Record{}
	for _, r := range s.exps {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return recKey(out[i].Network, out[i].ID) < recKey(out[j].Network, out[j].ID) })
	return out, nil
}

// Networks is a fixed set of networks.
type Networks []orchapi.Network

func (n Networks) Get(_ context.Context, name string) (orchapi.Network, error) {
	for _, x := range n {
		if x.Name == name {
			return x, nil
		}
	}
	return orchapi.Network{}, inventory.ErrNotFound
}

func (n Networks) List(context.Context) ([]orchapi.Network, error) { return n, nil }

// Members maps a network to the addresses of its live members.
type Members map[string][]string

func (m Members) Addresses(_ context.Context, network string) ([]string, error) {
	return m[network], nil
}

// Log discards events and counts them.
type Log struct {
	mu sync.Mutex
	N  int
}

func (l *Log) AppendEvent(context.Context, orchapi.Event) error {
	l.mu.Lock()
	l.N++
	l.mu.Unlock()
	return nil
}

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	T  time.Time
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.T
}

// Advance moves the clock.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.T = c.T.Add(d)
	c.mu.Unlock()
}

// Profile is the host profile's store: Raw, or none when it is empty.
type Profile struct{ Raw []byte }

// GetProfile returns Raw, and inventory.ErrNotFound when it is empty.
func (p Profile) GetProfile(context.Context) ([]byte, error) {
	if len(p.Raw) == 0 {
		return nil, inventory.ErrNotFound
	}
	return p.Raw, nil
}
