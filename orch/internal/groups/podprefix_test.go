package groups_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/groups"
)

// podSpec is a group whose members each take a /26 of network lab, a /24.
func podSpec(name string, count int) groups.GroupSpec {
	s := spec(name, count)
	s.Template.Network, s.Template.PodPrefixBits = "lab", 26
	return s
}

func labWorld(t *testing.T, prefix string) *world {
	w := newWorld(t)
	hostsABC(w)
	w.networks = []groups.Network{{Name: "lab", Prefix: prefix}}
	return w
}

// held returns the Pod prefixes of the group's live members, in member order.
func held(w *world, group string) []string {
	var out []string
	for _, m := range w.live(group) {
		out = append(out, m.PodPrefix)
	}
	return out
}

func TestPodPrefix_EachMemberHoldsItsOwnPrefixOfTheNetwork(t *testing.T) {
	w := labWorld(t, "10.9.0.0/24")
	w.setGroup(podSpec("web", 3))
	e := w.engine()
	w.settle(e, 5)

	assert.Equal(t, []string{"10.9.0.0/26", "10.9.0.64/26", "10.9.0.128/26"}, held(w, "web"))
	for _, m := range w.live("web") {
		fm, _ := w.machine(m.MachineID)
		require.NotNil(t, fm)
		assert.Equal(t, m.PodPrefix, fm.spec.PodPrefix, "the host is asked for the prefix the member holds")
		assert.Equal(t, "lab", m.Network)
	}
}

func TestPodPrefix_TwoGroupsOnOneNetworkNeverShareAPrefix(t *testing.T) {
	w := labWorld(t, "10.9.0.0/24")
	small := spec("db", 2)
	small.Template.Network, small.Template.PodPrefixBits = "lab", 27
	w.setGroup(podSpec("web", 2))
	w.setGroup(small)
	e := w.engine()
	w.settle(e, 5)

	var all []netip.Prefix
	for _, g := range []string{"web", "db"} {
		for _, p := range held(w, g) {
			all = append(all, netip.MustParsePrefix(p))
		}
	}
	require.Len(t, all, 4)
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			assert.Falsef(t, all[i].Overlaps(all[j]), "%s overlaps %s", all[i], all[j])
		}
	}
}

func TestPodPrefix_AGoneMembersPrefixIsFreedAndOnlyThen(t *testing.T) {
	w := labWorld(t, "10.9.0.0/25") // two /26
	w.setGroup(podSpec("web", 2))
	e := w.engine()
	w.settle(e, 5)
	require.Equal(t, []string{"10.9.0.0/26", "10.9.0.64/26"}, held(w, "web"))

	// A third member finds none free while both hold theirs.
	w.setGroup(podSpec("web", 3))
	plan := w.reconcile(e)
	assert.Zero(t, kinds(plan, groups.ActionCreate))
	assert.Contains(t, plan[len(plan)-1].Reason, "no free /26")

	// The first member's microVM is lost: its record goes gone and the replacement takes its prefix.
	w.setGroup(podSpec("web", 2))
	w.kill("web-1")
	w.settle(e, 5)
	assert.Equal(t, groups.PhaseGone, w.members["web-1"].Phase)
	assert.Equal(t, []string{"10.9.0.64/26", "10.9.0.0/26"}, held(w, "web"), "web-3 holds the prefix web-1 gave up")
}

func TestPodPrefix_ANetworkOrchDoesNotKeepHoldsTheGroup(t *testing.T) {
	w := labWorld(t, "10.9.0.0/24")
	w.networks = nil
	w.setGroup(podSpec("web", 1))
	plan := w.reconcile(w.engine())

	assert.Zero(t, kinds(plan, groups.ActionCreate))
	assert.Contains(t, plan[0].Reason, `network "lab" is not declared`)
	assert.Empty(t, w.live("web"))
}

func TestPodPrefix_AClaimLostToAnotherLeaderIsNotHandedOutTwice(t *testing.T) {
	w := labWorld(t, "10.9.0.0/24")
	w.setGroup(podSpec("web", 1))
	e := w.engine()
	ctx := context.Background()

	o, err := e.Observe(ctx)
	require.NoError(t, err)
	stale := groups.Plan(groups.Evaluate(o), o)
	require.Equal(t, "10.9.0.0/26", stale[0].Member.PodPrefix)

	// Another leader plans the same prefix for another member and wins.
	w.rev++
	w.members["other-1"] = groups.Member{ID: "other-1", Group: "other", Network: "lab", PodPrefix: "10.9.0.0/26", Phase: groups.PhasePending, Revision: w.rev}

	err = e.Apply(ctx, stale)
	require.ErrorIs(t, err, groups.ErrConflict)
	m, _ := w.machine("web-1")
	assert.Nil(t, m, "no microVM is made for a member whose claim was lost")
	assert.Empty(t, w.live("web"))

	// The next pass sees the other member's prefix and takes the next one.
	w.settle(e, 5)
	assert.Equal(t, []string{"10.9.0.64/26"}, held(w, "web"))
}
