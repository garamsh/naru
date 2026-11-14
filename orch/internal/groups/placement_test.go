package groups_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/groups"
)

func okHost(id string) groups.Host {
	return groups.Host{
		ID: id, Region: "kr-1", Ready: true, NodeVersions: []string{"v0.2.0", "v0.1.0"},
		Labels: map[string]string{"disk": "ssd"}, Capacity: groups.Capacity{CPUs: 8, MemoryMiB: 8192},
	}
}

func TestPlace_RefusesAHostThatBreaksAConstraint(t *testing.T) {
	s := spec("web", 1)
	s.RequiredLabels = map[string]string{"disk": "ssd"}
	s.Template.Form = groups.FormOS
	s.Template.NodeVersion = "v0.2.0"

	cases := []struct {
		name   string
		mutate func(*groups.Host)
		reason string
	}{
		{"outside the region", func(h *groups.Host) { h.Region = "kr-2" }, "region"},
		{"outside the node version window", func(h *groups.Host) { h.NodeVersions = []string{"v0.4.0", "v0.3.0"} }, "node version"},
		{"required label missing", func(h *groups.Host) { h.Labels = nil }, "required label"},
		{"required label of another value", func(h *groups.Host) { h.Labels = map[string]string{"disk": "hdd"} }, "required label"},
		{"draining", func(h *groups.Host) { h.Draining = true }, "draining"},
		{"not ready", func(h *groups.Host) { h.Ready = false }, "not ready"},
		{"no room", func(h *groups.Host) { h.Capacity.MemoryMiB = 100 }, "capacity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := okHost("bad")
			tc.mutate(&bad)

			control, err := groups.Place(s, []groups.Host{okHost("good")}, nil)
			require.NoError(t, err, "control: the unchanged host takes the member")
			assert.Equal(t, "good", control.ID)

			_, err = groups.Place(s, []groups.Host{bad}, nil)

			var pe *groups.PlacementError
			require.ErrorAs(t, err, &pe)
			require.ErrorIs(t, err, groups.ErrNoHost)
			assert.Contains(t, pe.Refused["bad"][0], tc.reason)

			got, err := groups.Place(s, []groups.Host{bad, okHost("good")}, nil)
			require.NoError(t, err)
			assert.Equal(t, "good", got.ID, "the refused host is skipped, not fatal")
		})
	}
}

func TestPlace_SpreadsBeforeItFillsAHost(t *testing.T) {
	s := spec("web", 3)
	got, err := groups.Place(s, []groups.Host{okHost("a"), okHost("b")}, map[string]int{"a": 1})
	require.NoError(t, err)
	assert.Equal(t, "b", got.ID)
}

func TestValidate(t *testing.T) {
	stateful := func(g *groups.GroupSpec) { g.Stateful, g.FenceTTL = true, 1 }
	cases := []struct {
		name   string
		mutate func(*groups.GroupSpec)
		ok     bool
	}{
		{"a plain group", func(*groups.GroupSpec) {}, true},
		{"a stateful group with a ttl", stateful, true},
		{"a stateful group without a ttl", func(g *groups.GroupSpec) { g.Stateful = true }, false},
		{"a stateful group that surges", func(g *groups.GroupSpec) { stateful(g); g.Rollout.MaxSurge = 1 }, false},
		{"no region", func(g *groups.GroupSpec) { g.Region = "" }, false},
		{"minAvailable above count", func(g *groups.GroupSpec) { g.MinAvailable = 4 }, false},
		{"a rollout that can never remove a member", func(g *groups.GroupSpec) { g.MinAvailable, g.Rollout = 3, groups.Rollout{} }, false},
		{"the same, with a surge", func(g *groups.GroupSpec) { g.MinAvailable, g.Rollout = 3, groups.Rollout{MaxSurge: 1} }, true},
		{"a name that is not a DNS label", func(g *groups.GroupSpec) { g.Name = "Web_1" }, false},
		{"no cpus", func(g *groups.GroupSpec) { g.Template.CPUs = 0 }, false},
		{"an unknown form", func(g *groups.GroupSpec) { g.Template.Form = "vm" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := spec("web", 3)
			tc.mutate(&g)
			err := g.Validate()
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, groups.ErrInvalidSpec)
		})
	}
}

func TestEngine_RefusesAnInvalidGroupAndRecordsWhy(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	bad := spec("web", 2)
	bad.Region = ""
	w.setGroup(bad)

	w.reconcile(w.engine())

	assert.Empty(t, w.live("web"))
	held := w.kinds(groups.EventHold)
	require.Len(t, held, 1)
	assert.Contains(t, held[0].Message, "region is empty")
}

func TestEngine_PlacementRefusalIsADecisionNotAFailure(t *testing.T) {
	w := newWorld(t)
	w.addHost("a", "kr-2", nil)
	s := spec("web", 1)
	w.setGroup(s)
	e := w.engine()

	w.reconcile(e)
	require.Empty(t, w.live("web"), "no host in the group's region")

	w.addHost("b", "kr-1", nil)
	w.settle(e, 3)
	require.Len(t, w.live("web"), 1)
	assert.Equal(t, "b", w.live("web")[0].Host)
}
