// Package groupsvc is what the API does with groups: it validates and stores a spec, and shows a
// group with its members and their phases. It never touches a microVM; the engine does.
package groupsvc

import (
	"context"
	"fmt"
	"sort"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/groups/wire"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// Event types the service writes.
const (
	EventApplied = "group.applied"
	EventDeleted = "group.deleted"
)

// Service is the groups API's operations.
type Service interface {
	// Apply creates the group or replaces its spec; by is the caller, for the event log.
	Apply(ctx context.Context, by string, spec orchapi.GroupSpec) (orchapi.Group, error)
	Get(ctx context.Context, name string) (orchapi.Group, error)
	List(ctx context.Context) ([]orchapi.Group, error)
	// Delete removes a group that has no live member, which count 0 brings about.
	Delete(ctx context.Context, by, name string) error
	// MicroVMs lists what the hosts that answer report, naming the group of each member's microVM.
	MicroVMs(ctx context.Context) ([]orchapi.MicroVM, error)
}

type service struct {
	store Store
	hosts groups.Hosts
	log   Log
	clock Clock
}

// New returns a Service. hosts may be nil, and then a member's phase is the stored one.
func New(store Store, hosts groups.Hosts, log Log, clock Clock) Service {
	return &service{store: store, hosts: hosts, log: log, clock: clock}
}

func (s *service) Apply(ctx context.Context, by string, a orchapi.GroupSpec) (orchapi.Group, error) {
	spec := wire.Spec(a)
	if err := spec.Validate(); err != nil {
		return orchapi.Group{}, fmt.Errorf("%w: %v", inventory.ErrInvalid, err)
	}
	if err := s.store.PutGroup(ctx, spec); err != nil {
		return orchapi.Group{}, err
	}
	ev := orchapi.Event{Time: s.clock.Now(), Type: EventApplied, Subject: spec.Name, Detail: map[string]string{"by": by, "template": spec.Template.Hash()}}
	if err := s.log.AppendEvent(ctx, ev); err != nil {
		return orchapi.Group{}, err
	}
	return s.one(ctx, spec)
}

func (s *service) Get(ctx context.Context, name string) (orchapi.Group, error) {
	spec, err := s.store.GetGroup(ctx, name)
	if err != nil {
		return orchapi.Group{}, err
	}
	return s.one(ctx, spec)
}

func (s *service) one(ctx context.Context, spec groups.GroupSpec) (orchapi.Group, error) {
	out, err := s.views(ctx, []groups.GroupSpec{spec})
	if err != nil {
		return orchapi.Group{}, err
	}
	return out[0], nil
}

func (s *service) List(ctx context.Context) ([]orchapi.Group, error) {
	specs, err := s.store.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	return s.views(ctx, specs)
}

func (s *service) Delete(ctx context.Context, by, name string) error {
	spec, err := s.store.GetGroup(ctx, name)
	if err != nil {
		return err
	}
	members, err := s.store.ListMembers(ctx)
	if err != nil {
		return err
	}
	for _, m := range members {
		if m.Group == spec.Name && m.Phase != groups.PhaseGone {
			return fmt.Errorf("%w: group %s still has member %s; apply it with count 0 and delete it once its members are gone", inventory.ErrConflict, name, m.ID)
		}
	}
	if err := s.store.DeleteGroup(ctx, name); err != nil {
		return err
	}
	return s.log.AppendEvent(ctx, orchapi.Event{Time: s.clock.Now(), Type: EventDeleted, Subject: name, Detail: map[string]string{"by": by}})
}

func (s *service) MicroVMs(ctx context.Context) ([]orchapi.MicroVM, error) {
	out := []orchapi.MicroVM{}
	if s.hosts == nil {
		return out, nil
	}
	members, err := s.store.ListMembers(ctx)
	if err != nil {
		return nil, err
	}
	type key struct{ host, machine string }
	groupOf := map[key]string{}
	for _, m := range members {
		groupOf[key{m.Host, m.MachineID}] = m.Group
	}
	hosts, err := s.hosts.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		for _, m := range h.Machines {
			out = append(out, orchapi.MicroVM{Host: h.ID, Name: m.ID, State: string(m.State), Group: groupOf[key{h.ID, m.ID}]})
		}
	}
	return out, nil
}

// views shows each spec with its members. Phases are computed from the hosts when they answer,
// and are the stored ones otherwise.
func (s *service) views(ctx context.Context, specs []groups.GroupSpec) ([]orchapi.Group, error) {
	members, err := s.store.ListMembers(ctx)
	if err != nil {
		return nil, err
	}
	o := groups.Observed{Now: s.clock.Now(), Groups: specs, Members: members, Fences: map[string]groups.Fence{}}
	observed := false
	if s.hosts != nil {
		if o.Hosts, err = s.hosts.List(ctx); err == nil {
			observed = true
		}
	}
	out := make([]orchapi.Group, 0, len(specs))
	for _, gs := range groups.Evaluate(o).Groups {
		g := orchapi.Group{Spec: wire.API(gs.Spec), Hash: gs.Hash, Observed: observed, Members: []orchapi.GroupMember{}}
		if gs.Invalid != nil {
			g.Invalid = gs.Invalid.Error()
		}
		for _, m := range gs.Members {
			phase := m.Member.Phase
			if observed {
				phase = m.Phase
			}
			g.Members = append(g.Members, orchapi.GroupMember{ID: m.ID, Host: m.Host, TemplateHash: m.TemplateHash, Phase: string(phase), LastSeen: m.LastSeen})
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out, nil
}
