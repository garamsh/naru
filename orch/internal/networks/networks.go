// Package networks is what the API does with networks: it validates and stores a network, and
// refuses to remove one a group still uses. Putting a network on the hosts is the reconciler's.
package networks

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// Event types the service writes.
const (
	EventApplied = "network.applied"
	EventDeleted = "network.deleted"
)

// Store holds the networks. Get and Delete return inventory.ErrNotFound for a network that is not
// there.
type Store interface {
	Put(ctx context.Context, n orchapi.Network) error
	Get(ctx context.Context, name string) (orchapi.Network, error)
	Delete(ctx context.Context, name string) error
	// List returns every network in name order.
	List(ctx context.Context) ([]orchapi.Network, error)
}

// Groups lists the groups, to find the ones that use a network.
type Groups interface {
	ListGroups(ctx context.Context) ([]groups.GroupSpec, error)
}

// Log is the inventory's event log.
type Log interface {
	AppendEvent(ctx context.Context, ev orchapi.Event) error
}

// Clock is the service's time.
type Clock interface {
	Now() time.Time
}

// nameRe is what a host accepts as the name of a network.
var nameRe = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Validate reports why the network cannot be kept, as an error that wraps inventory.ErrInvalid.
func Validate(n orchapi.Network) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", inventory.ErrInvalid, fmt.Sprintf(format, a...))
	}
	if !nameRe.MatchString(n.Name) {
		return bad("network name %q is not a DNS label", n.Name)
	}
	p, err := netip.ParsePrefix(n.Prefix)
	if err != nil || !p.Addr().Is4() || p != p.Masked() {
		return bad("prefix %q is not an IPv4 prefix with its host bits clear", n.Prefix)
	}
	if n.APIEndpoint != "" {
		a, err := netip.ParseAddr(n.APIEndpoint)
		if err != nil || !p.Contains(a) {
			return bad("apiEndpoint %q is not an address inside %s", n.APIEndpoint, p)
		}
	}
	return nil
}

// Service is the networks API's operations.
type Service struct {
	store  Store
	groups Groups
	log    Log
	clock  Clock
}

// New returns a Service.
func New(store Store, groups Groups, log Log, clock Clock) *Service {
	return &Service{store: store, groups: groups, log: log, clock: clock}
}

// Apply creates the network or replaces its spec; by is the caller, for the event log. A network
// that a group uses keeps its prefix, since the members' Pod prefixes were cut from it.
func (s *Service) Apply(ctx context.Context, by, name string, spec orchapi.NetworkSpec) (orchapi.Network, error) {
	n := orchapi.Network{Name: name, Prefix: spec.Prefix, APIEndpoint: spec.APIEndpoint}
	if err := Validate(n); err != nil {
		return orchapi.Network{}, err
	}
	old, err := s.store.Get(ctx, name)
	switch {
	case err == nil && old.Prefix != n.Prefix:
		users, err := s.users(ctx, name)
		if err != nil {
			return orchapi.Network{}, err
		}
		if len(users) > 0 {
			return orchapi.Network{}, fmt.Errorf("%w: group %s uses network %s, so its prefix %s cannot change", inventory.ErrConflict, users[0], name, old.Prefix)
		}
	case err != nil && !errors.Is(err, inventory.ErrNotFound):
		return orchapi.Network{}, err
	}
	if err := s.store.Put(ctx, n); err != nil {
		return orchapi.Network{}, err
	}
	ev := orchapi.Event{Time: s.clock.Now(), Type: EventApplied, Subject: name, Detail: map[string]string{"by": by, "prefix": n.Prefix}}
	return n, s.log.AppendEvent(ctx, ev)
}

// Get returns the network.
func (s *Service) Get(ctx context.Context, name string) (orchapi.Network, error) {
	return s.store.Get(ctx, name)
}

// List returns every network.
func (s *Service) List(ctx context.Context) ([]orchapi.Network, error) {
	return s.store.List(ctx)
}

// Delete removes a network no group uses.
func (s *Service) Delete(ctx context.Context, by, name string) error {
	if _, err := s.store.Get(ctx, name); err != nil {
		return err
	}
	users, err := s.users(ctx, name)
	if err != nil {
		return err
	}
	if len(users) > 0 {
		return fmt.Errorf("%w: group %s uses network %s", inventory.ErrConflict, users[0], name)
	}
	if err := s.store.Delete(ctx, name); err != nil {
		return err
	}
	return s.log.AppendEvent(ctx, orchapi.Event{Time: s.clock.Now(), Type: EventDeleted, Subject: name, Detail: map[string]string{"by": by}})
}

// users returns the names of the groups whose template joins the network.
func (s *Service) users(ctx context.Context, name string) ([]string, error) {
	gs, err := s.groups.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, g := range gs {
		if g.Template.Network == name {
			out = append(out, g.Name)
		}
	}
	return out, nil
}
