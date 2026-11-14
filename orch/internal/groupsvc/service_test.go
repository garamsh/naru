package groupsvc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/groups/wire"
	"github.com/garamsh/naru/orch/internal/groupsvc"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

type store struct {
	groups  map[string]groups.GroupSpec
	members map[string]groups.Member
}

func (s *store) ListGroups(context.Context) ([]groups.GroupSpec, error) {
	var out []groups.GroupSpec
	for _, g := range s.groups {
		out = append(out, g)
	}
	return out, nil
}

func (s *store) ListMembers(context.Context) ([]groups.Member, error) {
	var out []groups.Member
	for _, m := range s.members {
		out = append(out, m)
	}
	return out, nil
}

func (s *store) PutMember(_ context.Context, m groups.Member) (groups.Member, error) {
	s.members[m.ID] = m
	return m, nil
}

func (s *store) PutGroup(_ context.Context, g groups.GroupSpec) error {
	s.groups[g.Name] = g
	return nil
}

func (s *store) GetGroup(_ context.Context, name string) (groups.GroupSpec, error) {
	g, ok := s.groups[name]
	if !ok {
		return groups.GroupSpec{}, inventory.ErrNotFound
	}
	return g, nil
}

func (s *store) DeleteGroup(_ context.Context, name string) error {
	delete(s.groups, name)
	return nil
}

type log struct{ events []orchapi.Event }

func (l *log) AppendEvent(_ context.Context, ev orchapi.Event) error {
	l.events = append(l.events, ev)
	return nil
}

type clock struct{}

func (clock) Now() time.Time { return time.Unix(5000, 0).UTC() }

type deadHosts struct{ *orchtest.FakeHosts }

func (deadHosts) List(context.Context) ([]groups.Host, error) { return nil, errors.New("no answer") }

func spec() orchapi.GroupSpec {
	return orchapi.GroupSpec{
		Name: "web", Count: 2, Region: "kr-seoul",
		Template: orchapi.GroupTemplate{Form: "image", CPUs: 1, MemoryMiB: 128, Version: "r@sha256:abc"},
		Rollout:  orchapi.GroupRollout{MaxUnavailable: 1},
	}
}

func newSvc(h groups.Hosts) (groupsvc.Service, *store, *log) {
	st := &store{groups: map[string]groups.GroupSpec{}, members: map[string]groups.Member{}}
	l := &log{}
	return groupsvc.New(st, h, l, clock{}), st, l
}

func TestApplyRefusesAnInvalidSpecAndStoresAndLogsAValidOne(t *testing.T) {
	svc, st, l := newSvc(nil)
	bad := spec()
	bad.Count = -1
	_, err := svc.Apply(t.Context(), "alice", bad)
	require.ErrorIs(t, err, inventory.ErrInvalid)
	require.Empty(t, st.groups)
	require.Empty(t, l.events)

	g, err := svc.Apply(t.Context(), "alice", spec())
	require.NoError(t, err)
	require.Equal(t, "web", g.Spec.Name)
	require.NotEmpty(t, g.Hash)
	require.Contains(t, st.groups, "web")
	require.Len(t, l.events, 1)
	require.Equal(t, groupsvc.EventApplied, l.events[0].Type)
	require.Equal(t, "alice", l.events[0].Detail["by"])
}

func TestDeleteIsRefusedWhileAMemberIsNotGone(t *testing.T) {
	svc, st, l := newSvc(nil)
	_, err := svc.Apply(t.Context(), "alice", spec())
	require.NoError(t, err)
	st.members["web-1"] = groups.Member{ID: "web-1", Group: "web", Phase: groups.PhaseRunning}

	require.ErrorIs(t, svc.Delete(t.Context(), "alice", "web"), inventory.ErrConflict)
	require.Contains(t, st.groups, "web")

	st.members["web-1"] = groups.Member{ID: "web-1", Group: "web", Phase: groups.PhaseGone}
	require.NoError(t, svc.Delete(t.Context(), "alice", "web"))
	require.NotContains(t, st.groups, "web")
	require.Equal(t, groupsvc.EventDeleted, l.events[len(l.events)-1].Type)

	require.ErrorIs(t, svc.Delete(t.Context(), "alice", "web"), inventory.ErrNotFound)
	_, err = svc.Get(t.Context(), "web")
	require.ErrorIs(t, err, inventory.ErrNotFound)
}

func TestPhasesAreComputedFromTheHostsOrElseTheStoredOnes(t *testing.T) {
	fh := orchtest.NewFakeHosts("h1")
	svc, st, _ := newSvc(fh)
	_, err := svc.Apply(t.Context(), "alice", spec())
	require.NoError(t, err)
	hash := wire.Spec(spec()).Template.Hash()
	st.members["web-1"] = groups.Member{ID: "web-1", Group: "web", Host: "h1", MachineID: "web-1", TemplateHash: hash, Phase: groups.PhasePending}
	require.NoError(t, fh.Create(t.Context(), "h1", groups.MachineSpec{ID: "web-1"}))

	g, err := svc.Get(t.Context(), "web")
	require.NoError(t, err)
	require.True(t, g.Observed)
	require.Len(t, g.Members, 1)
	require.Equal(t, orchapi.PhaseRunning, g.Members[0].Phase)

	// When the hosts do not answer the stored phase is shown, and the view says it was not observed.
	svc, st2, _ := newSvc(deadHosts{fh})
	st2.groups["web"] = wire.Spec(spec())
	st2.members["web-1"] = st.members["web-1"]
	g, err = svc.Get(t.Context(), "web")
	require.NoError(t, err)
	require.False(t, g.Observed)
	require.Equal(t, string(groups.PhasePending), g.Members[0].Phase)

	all, err := svc.List(t.Context())
	require.NoError(t, err)
	require.Len(t, all, 1)
}
