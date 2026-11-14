package groups_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/groups"
)

func hostsABC(w *world) {
	w.addHost("a", "kr-1", map[string]string{"disk": "ssd"})
	w.addHost("b", "kr-1", map[string]string{"disk": "ssd"})
	w.addHost("c", "kr-1", map[string]string{"disk": "ssd"})
}

func kinds(plan []groups.Action, k groups.ActionKind) int {
	n := 0
	for _, a := range plan {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func TestCountKept_ReplacesAKilledMember(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	w.setGroup(spec("web", 3))
	e := w.engine()

	w.settle(e, 5)
	require.Equal(t, 3, w.running("web"))
	assert.Empty(t, kinds(w.reconcile(e), groups.ActionCreate), "a full group plans no create")

	w.kill("web-2")
	require.Equal(t, 2, w.running("web"), "setup: the kill took effect")
	plan := w.reconcile(e)

	assert.Equal(t, 1, kinds(plan, groups.ActionCreate))
	assert.Equal(t, 1, kinds(plan, groups.ActionForget))
	assert.Equal(t, 3, w.running("web"))
	assert.Len(t, w.live("web"), 3)
	assert.Equal(t, groups.PhaseGone, w.members["web-2"].Phase, "the killed member is forgotten")
}

func TestCountKept_ReplacesMembersOfALostHostAtOnce(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	w.setGroup(spec("web", 3))
	e := w.engine()
	w.settle(e, 5)

	w.hosts["a"].down = true
	plan := w.reconcile(e)

	assert.Equal(t, 1, kinds(plan, groups.ActionCreate), "a stateless member needs no fence wait")
	for _, m := range w.live("web") {
		assert.NotEqual(t, "a", m.Host)
	}
}

func TestCountKept_ScalesDown(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	w.setGroup(spec("web", 3))
	e := w.engine()
	w.settle(e, 5)

	w.setGroup(spec("web", 1))
	w.settle(e, 5)

	assert.Equal(t, 1, w.running("web"))
	assert.Len(t, w.live("web"), 1)
}

func TestCreate_FailureIsRecordedAndLeavesNothingBehind(t *testing.T) {
	w := newWorld(t)
	w.addHost("a", "kr-1", nil).failCreate = true
	w.setGroup(spec("web", 1))
	e := w.engine()

	o, err := e.Observe(context.Background())
	require.NoError(t, err)
	err = e.Apply(context.Background(), groups.Plan(groups.Evaluate(o), o))

	require.Error(t, err)
	assert.Len(t, w.kinds(groups.EventFailed), 1)
	assert.Empty(t, w.live("web"), "the record of the failed create is forgotten")
	m, _ := w.machine("web-1")
	assert.Nil(t, m)
}

func TestRollingUpdate_KeepsMinAvailableAtEveryStep(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	s := spec("web", 3)
	s.MinAvailable = 2
	s.Rollout = groups.Rollout{MaxUnavailable: 1, MaxSurge: 1}
	w.setGroup(s)
	e := w.engine()
	w.settle(e, 5)
	oldHash := s.Template.Hash()

	steps := 0
	w.check = func() {
		steps++
		assert.GreaterOrEqual(t, w.running("web"), 2, "available fell below minAvailable at step %d", steps)
	}
	s.Template.Version = "v2"
	w.setGroup(s)
	passes := w.settle(e, 12)

	t.Logf("rolled in %d passes, %d machine changes", passes, steps)
	require.Greater(t, steps, 3, "setup: the rollout changed machines")
	live := w.live("web")
	require.Len(t, live, 3)
	for _, m := range live {
		assert.Equal(t, s.Template.Hash(), m.TemplateHash)
		assert.NotEqual(t, oldHash, m.TemplateHash)
	}
	assert.Equal(t, 3, w.running("web"))
}

func TestRollingUpdate_AFreshEngineFinishesWhatAnotherStarted(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	s := spec("web", 3)
	s.MinAvailable = 2
	s.Rollout = groups.Rollout{MaxUnavailable: 1, MaxSurge: 1}
	w.setGroup(s)
	w.settle(w.engine(), 5)
	s.Template.Version = "v2"
	w.setGroup(s)

	w.reconcile(w.engine())
	w.reconcile(w.engine())
	w.settle(w.engine(), 12)

	for _, m := range w.live("web") {
		assert.Equal(t, s.Template.Hash(), m.TemplateHash)
	}
	assert.Equal(t, 3, w.running("web"))
}

func TestPlan_DependsOnlyOnObservation(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	s := spec("web", 3)
	w.setGroup(s)
	e := w.engine()
	w.settle(e, 5)
	w.kill("web-1")

	o, err := e.Observe(context.Background())
	require.NoError(t, err)

	assert.Equal(t, groups.Plan(groups.Evaluate(o), o), groups.Plan(groups.Evaluate(o), o))
}

func TestDrain_MovesMembersOneAtATimeWithinMinAvailable(t *testing.T) {
	w := newWorld(t)
	w.addHost("a", "kr-1", nil)
	w.addHost("b", "kr-1", nil)
	s := spec("web", 4)
	s.MinAvailable = 3
	w.setGroup(s)
	e := w.engine()
	w.settle(e, 5)
	require.Equal(t, 2, len(w.hosts["a"].machines), "setup: two members on the host to drain")

	w.check = func() { assert.GreaterOrEqual(t, w.running("web"), 3) }
	w.hosts["a"].host.Draining = true
	for range 10 {
		plan := w.reconcile(e)
		assert.LessOrEqual(t, kinds(plan, groups.ActionDelete), 1, "at most one member leaves per pass")
		if len(plan) == kinds(plan, groups.ActionHold) {
			break
		}
	}

	assert.Empty(t, w.hosts["a"].machines)
	assert.Equal(t, 4, w.running("web"))
	for _, m := range w.live("web") {
		assert.Equal(t, "b", m.Host)
	}
}

func TestRequiredLabel_BrokenLabelReplacesTheMember(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	s := spec("web", 2)
	s.RequiredLabels = map[string]string{"disk": "ssd"}
	s.MinAvailable = 1
	w.setGroup(s)
	e := w.engine()
	w.settle(e, 5)
	onA, onB := w.hosts["a"].machines, w.hosts["b"].machines
	require.Len(t, onA, 1, "setup: a member on a")
	require.Len(t, onB, 1, "setup: a member on b")
	keptID := ""
	for id := range onB {
		keptID = id
	}

	w.hosts["a"].host.Labels = map[string]string{"disk": "hdd"}
	w.settle(e, 8)

	assert.Empty(t, w.hosts["a"].machines, "the member left the host that lost the label")
	assert.Len(t, w.hosts["c"].machines, 1, "and was placed on the host that has it")
	assert.Contains(t, w.hosts["b"].machines, keptID, "a member on a host that kept the label stays")
	assert.Equal(t, 2, w.running("web"))
}

func TestPreferredLabels_OrderPlacement(t *testing.T) {
	cases := []struct {
		name string
		pref []groups.LabelPreference
		want string
	}{
		{"no preference falls to the lowest ID", nil, "a"},
		{"a preference moves it", []groups.LabelPreference{{Key: "zone", Value: "x", Weight: 10}}, "b"},
		{"the heavier of two wins", []groups.LabelPreference{
			{Key: "zone", Value: "x", Weight: 10}, {Key: "tier", Value: "gold", Weight: 30},
		}, "c"},
		{"weights add", []groups.LabelPreference{
			{Key: "zone", Value: "x", Weight: 10}, {Key: "tier", Value: "gold", Weight: 5}, {Key: "zone", Value: "y", Weight: 20},
		}, "c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.addHost("a", "kr-1", nil)
			w.addHost("b", "kr-1", map[string]string{"zone": "x"})
			w.addHost("c", "kr-1", map[string]string{"tier": "gold", "zone": "y"})
			s := spec("web", 1)
			s.PreferredLabels = tc.pref
			w.setGroup(s)

			w.settle(w.engine(), 3)

			require.Len(t, w.live("web"), 1)
			assert.Equal(t, tc.want, w.live("web")[0].Host)
		})
	}
}

func statefulSpec() groups.GroupSpec {
	s := spec("db", 1)
	s.Stateful = true
	s.FenceTTL = 10 * time.Minute
	s.Rollout = groups.Rollout{MaxUnavailable: 1}
	return s
}

func statefulWorld(t *testing.T) (*world, *groups.Engine) {
	w := newWorld(t)
	w.addHost("a", "kr-1", nil)
	w.addHost("b", "kr-1", nil)
	w.setGroup(statefulSpec())
	e := w.engine()
	w.settle(e, 5)
	return w, e
}

func TestStatefulFencing_CreatedFencedAndRenewedWhileHealthy(t *testing.T) {
	w, e := statefulWorld(t)

	m, host := w.machine("db-1")
	require.NotNil(t, m)
	assert.True(t, m.spec.Fenced, "a stateful member is created fenced")
	assert.Equal(t, groups.MachineRunning, m.state, "and starts once its fence is armed")
	first := m.deadline
	assert.Equal(t, t0.Add(10*time.Minute), first)

	w.advance(2 * time.Minute)
	assert.Equal(t, 0, kinds(w.reconcile(e), groups.ActionRenew), "a fence with most of its time left is not renewed")
	w.advance(4 * time.Minute)
	w.reconcile(e)

	assert.Equal(t, w.now.Add(10*time.Minute), m.deadline, "renewed from the pass's time")
	assert.Equal(t, m.deadline, w.members["db-1"].FenceDeadline, "and recorded on the member")
	assert.Equal(t, "a", host)
}

func TestStatefulFencing_ReplacementWaitsForTheDeadline(t *testing.T) {
	w, e := statefulWorld(t)
	w.advance(6 * time.Minute)
	w.reconcile(e)
	deadline := w.members["db-1"].FenceDeadline
	require.False(t, deadline.IsZero())

	w.hosts["a"].down = true
	creates := 0
	for w.now.Before(deadline.Add(-time.Minute)) {
		plan := w.reconcile(e)
		creates += kinds(plan, groups.ActionCreate)
		w.advance(time.Minute)
	}
	require.Equal(t, 0, creates, "no replacement while the old fence deadline has not passed")
	require.NotEmpty(t, w.kinds(groups.EventHold), "the wait is recorded as a decision")

	w.advance(time.Minute)
	require.False(t, w.now.After(deadline), "setup: exactly at the deadline is not after it")
	assert.Equal(t, 0, kinds(w.reconcile(e), groups.ActionCreate))

	w.advance(time.Second)
	plan := w.reconcile(e)

	require.Equal(t, 1, kinds(plan, groups.ActionCreate))
	m, host := w.machine("db-2")
	require.NotNil(t, m)
	assert.Equal(t, "b", host)
	assert.True(t, m.createdAt.After(deadline), "the replacement's Create (%s) is strictly after the old deadline (%s)", m.createdAt, deadline)
	assert.True(t, m.spec.Fenced)
}

func TestStatefulFencing_ReplacementFollowsAReportedLapse(t *testing.T) {
	w, e := statefulWorld(t)
	w.hosts["a"].host.Ready = false
	w.reconcile(e)
	require.Len(t, w.kinds(groups.EventCreate), 1, "control: a host that is not ready, with no lapse reported, holds the replacement")
	require.Equal(t, 1, w.running("db"))

	old, _ := w.machine("db-1")
	require.True(t, w.now.Before(old.deadline), "setup: the deadline orch armed has not passed")
	old.state, old.lapsedAt = groups.MachineStopped, w.now
	plan := w.reconcile(e)

	assert.Equal(t, 1, kinds(plan, groups.ActionCreate), "the host's report of a lapse is enough")
	_, host := w.machine("db-2")
	assert.Equal(t, "b", host)
}

func TestStatefulFencing_ADrainStopsTheMemberBeforeItIsDeleted(t *testing.T) {
	w, e := statefulWorld(t)
	w.hosts["a"].host.Draining = true

	first := w.reconcile(e)
	assert.Equal(t, 1, kinds(first, groups.ActionStop))
	assert.Equal(t, 0, kinds(first, groups.ActionCreate), "a stateful member does not surge: no replacement yet")
	second := w.reconcile(e)
	assert.Equal(t, 1, kinds(second, groups.ActionDelete))
	w.settle(e, 5)

	_, host := w.machine("db-2")
	assert.Equal(t, "b", host)
}

func TestEvents_AreRecordedForEveryActionAndDecision(t *testing.T) {
	w := newWorld(t)
	w.addHost("a", "kr-2", nil)
	w.setGroup(spec("web", 1))
	e := w.engine()

	plan := w.reconcile(e)
	require.Equal(t, 1, kinds(plan, groups.ActionHold), "setup: no host in the region, so the planner decides to wait")
	held := w.kinds(groups.EventHold)
	require.Len(t, held, 1)
	assert.Contains(t, held[0].Message, "region")

	w.addHost("b", "kr-1", nil)
	w.reconcile(e)
	w.kill("web-1")
	w.reconcile(e)

	for _, k := range []groups.EventKind{groups.EventCreate, groups.EventForget, groups.EventHold} {
		assert.NotEmpty(t, w.kinds(k), "an event of kind %s", k)
	}
}

func TestRun_ReconcilesFromFreshObservationEveryPass(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	w.setGroup(spec("web", 3))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.afterWait = func(n int) {
		switch n {
		case 2:
			require.Equal(t, 3, w.running("web"), "setup: the first passes filled the group")
			w.kill("web-1")
		case 4:
			cancel()
		}
	}

	err := w.engine().Run(ctx)

	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 3, w.running("web"), "the member killed between passes was replaced")
	assert.Equal(t, 4, w.waits)
}

func TestRun_AFailedPassIsRecordedAndTheLoopContinues(t *testing.T) {
	w := newWorld(t)
	hostsABC(w)
	w.setGroup(spec("web", 1))
	for _, h := range w.hosts {
		h.failCreate = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.afterWait = func(n int) {
		switch n {
		case 1:
			for _, h := range w.hosts {
				h.failCreate = false
			}
		case 3:
			cancel()
		}
	}

	err := w.engine().Run(ctx)

	assert.ErrorIs(t, err, context.Canceled)
	assert.NotEmpty(t, w.kinds(groups.EventPassFailed), "the failed pass is recorded")
	assert.Equal(t, 1, w.running("web"), "and the next pass created the member")
}
