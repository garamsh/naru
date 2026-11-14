package networks_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/networks"
)

type store struct{ nets map[string]orchapi.Network }

func (s *store) Put(_ context.Context, n orchapi.Network) error { s.nets[n.Name] = n; return nil }
func (s *store) Get(_ context.Context, name string) (orchapi.Network, error) {
	n, ok := s.nets[name]
	if !ok {
		return orchapi.Network{}, inventory.ErrNotFound
	}
	return n, nil
}

func (s *store) Delete(_ context.Context, name string) error {
	if _, ok := s.nets[name]; !ok {
		return inventory.ErrNotFound
	}
	delete(s.nets, name)
	return nil
}

func (s *store) List(context.Context) ([]orchapi.Network, error) {
	var out []orchapi.Network
	for _, n := range s.nets {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type groupList []groups.GroupSpec

func (g groupList) ListGroups(context.Context) ([]groups.GroupSpec, error) { return g, nil }

type log struct{ events []orchapi.Event }

func (l *log) AppendEvent(_ context.Context, ev orchapi.Event) error {
	l.events = append(l.events, ev)
	return nil
}

type clock struct{}

func (clock) Now() time.Time { return time.Unix(7000, 0).UTC() }

func newSvc(gs ...groups.GroupSpec) (*networks.Service, *store, *log) {
	st := &store{nets: map[string]orchapi.Network{}}
	l := &log{}
	return networks.New(st, groupList(gs), l, clock{}), st, l
}

func TestApplyStoresAndLogsAValidNetwork(t *testing.T) {
	svc, st, l := newSvc()
	n, err := svc.Apply(t.Context(), "alice", "lab", orchapi.NetworkSpec{Prefix: "10.9.0.0/24", APIEndpoint: "10.9.0.2"})
	require.NoError(t, err)
	require.Equal(t, orchapi.Network{Name: "lab", Prefix: "10.9.0.0/24", APIEndpoint: "10.9.0.2"}, n)
	require.Equal(t, n, st.nets["lab"])
	require.Len(t, l.events, 1)
	require.Equal(t, networks.EventApplied, l.events[0].Type)
	require.Equal(t, "alice", l.events[0].Detail["by"])

	got, err := svc.Get(t.Context(), "lab")
	require.NoError(t, err)
	require.Equal(t, n, got)
	all, err := svc.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, []orchapi.Network{n}, all)
}

func TestApplyRefusesWhatAHostWouldRefuse(t *testing.T) {
	good := orchapi.NetworkSpec{Prefix: "10.9.0.0/24", APIEndpoint: "10.9.0.2"}
	for name, c := range map[string]struct {
		net  string
		spec orchapi.NetworkSpec
	}{
		"an upper-case name":          {"Lab", good},
		"an empty name":               {"", good},
		"a name with a slash":         {"a/b", good},
		"no prefix":                   {"lab", orchapi.NetworkSpec{}},
		"a prefix with host bits set": {"lab", orchapi.NetworkSpec{Prefix: "10.9.0.1/24"}},
		"an IPv6 prefix":              {"lab", orchapi.NetworkSpec{Prefix: "fd00::/64"}},
		"an endpoint outside":         {"lab", orchapi.NetworkSpec{Prefix: "10.9.0.0/24", APIEndpoint: "10.10.0.2"}},
		"an endpoint that is no IP":   {"lab", orchapi.NetworkSpec{Prefix: "10.9.0.0/24", APIEndpoint: "api"}},
	} {
		t.Run(name, func(t *testing.T) {
			svc, st, l := newSvc()
			_, err := svc.Apply(t.Context(), "alice", c.net, c.spec)
			require.ErrorIs(t, err, inventory.ErrInvalid)
			require.Empty(t, st.nets)
			require.Empty(t, l.events)
		})
	}

	// The control: the spec the table varies is accepted as it stands.
	svc, _, _ := newSvc()
	_, err := svc.Apply(t.Context(), "alice", "lab", good)
	require.NoError(t, err)
}

func usedBy(network string) groups.GroupSpec {
	return groups.GroupSpec{Name: "web", Template: groups.Template{Network: network}}
}

func TestAGroupsNetworkKeepsItsPrefixAndCannotBeDeleted(t *testing.T) {
	svc, st, l := newSvc(usedBy("lab"))
	_, err := svc.Apply(t.Context(), "alice", "lab", orchapi.NetworkSpec{Prefix: "10.9.0.0/24"})
	require.NoError(t, err)

	_, err = svc.Apply(t.Context(), "alice", "lab", orchapi.NetworkSpec{Prefix: "10.8.0.0/24"})
	require.ErrorIs(t, err, inventory.ErrConflict)
	require.Equal(t, "10.9.0.0/24", st.nets["lab"].Prefix)

	_, err = svc.Apply(t.Context(), "alice", "lab", orchapi.NetworkSpec{Prefix: "10.9.0.0/24", APIEndpoint: "10.9.0.2"})
	require.NoError(t, err, "the endpoint may change, the prefix may not")

	require.ErrorIs(t, svc.Delete(t.Context(), "alice", "lab"), inventory.ErrConflict)
	require.Contains(t, st.nets, "lab")
	events := len(l.events)

	// A network no group uses is free to change and to go.
	_, err = svc.Apply(t.Context(), "alice", "free", orchapi.NetworkSpec{Prefix: "10.7.0.0/24"})
	require.NoError(t, err)
	_, err = svc.Apply(t.Context(), "alice", "free", orchapi.NetworkSpec{Prefix: "10.6.0.0/24"})
	require.NoError(t, err)
	require.NoError(t, svc.Delete(t.Context(), "alice", "free"))
	require.NotContains(t, st.nets, "free")
	require.Equal(t, networks.EventDeleted, l.events[len(l.events)-1].Type)
	require.Greater(t, len(l.events), events)

	require.ErrorIs(t, svc.Delete(t.Context(), "alice", "free"), inventory.ErrNotFound)
}
