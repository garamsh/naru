package orchd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

const image = "registry.example/web@sha256:0000000000000000000000000000000000000000000000000000000000000001"

func webGroup() orchapi.GroupSpec {
	return orchapi.GroupSpec{
		Name: "web", Count: 3, Region: "kr-seoul",
		Template: orchapi.GroupTemplate{Form: "image", CPUs: 1, MemoryMiB: 128, Version: image, RootDiskBytes: 1 << 30},
		Rollout:  orchapi.GroupRollout{MaxUnavailable: 1},
	}
}

// waitRunning waits until the group shows want running members through the API of member i, all on
// hosts that answer, and the hosts hold exactly want running microVMs.
func waitRunning(t *testing.T, cl *orchapi.Client, hosts *orchtest.FakeHosts, want int) orchapi.Group {
	t.Helper()
	var g orchapi.Group
	require.Eventually(t, func() bool {
		var err error
		if g, err = cl.Group(t.Context(), "web"); err != nil {
			return false
		}
		running := 0
		for _, m := range g.Members {
			if m.Phase == orchapi.PhaseRunning {
				running++
			}
		}
		return running == want && len(hosts.Running()) == want
	}, 30*time.Second, 100*time.Millisecond, "the group did not reach %d running members", want)
	return g
}

func TestGroupIsKeptAtItsCountReplacedAfterItsHostFailsAndContinuedByTheNextLeader(t *testing.T) {
	hosts := orchtest.NewFakeHosts("h1", "h2", "h3", "h4")
	c := orchtest.Start(t, orchtest.Options{Members: 3, NewHosts: func(inventory.Service) groups.Hosts { return hosts }})
	operator := c.CA.Issue(t, "alice", identity.RoleOperator)
	all := []int{0, 1, 2}
	leader := c.Leader(t, all, 30*time.Second)
	cl := c.Client(t, leader, &operator)

	g, err := cl.ApplyGroup(t.Context(), webGroup())
	require.NoError(t, err)
	require.Equal(t, 3, g.Spec.Count)
	g = waitRunning(t, cl, hosts, 3)
	require.Len(t, g.Members, 3)
	require.True(t, g.Observed)

	// A microVM deleted behind the engine's back is made again.
	victim := hosts.Running()[0]
	hostID, machineID := victim[:2], victim[3:]
	hosts.Remove(hostID, machineID)
	waitRunning(t, cl, hosts, 3)

	// A member whose host fails is replaced on a host that answers.
	hosts.Fail(hostID)
	g = waitRunning(t, cl, hosts, 3)
	for _, m := range g.Members {
		if m.Phase == orchapi.PhaseRunning {
			require.NotEqual(t, hostID, m.Host)
		}
	}

	// The leader is killed: the member that takes over continues from what the store holds.
	require.NoError(t, c.Members[leader].Stop())
	var live []int
	for _, i := range all {
		if i != leader {
			live = append(live, i)
		}
	}
	next := c.Leader(t, live, failover)
	cl = c.Client(t, next, &operator)
	second := hosts.Running()[0]
	hosts.Fail(second[:2])
	g = waitRunning(t, cl, hosts, 3)
	for _, m := range g.Members {
		if m.Phase == orchapi.PhaseRunning {
			require.NotContains(t, []string{hostID, second[:2]}, m.Host)
		}
	}

	// Kept at its count: nothing more is made while it holds.
	require.Never(t, func() bool { return len(hosts.Running()) != 3 }, 2*time.Second, 100*time.Millisecond)
}

func TestGroupAPIRolesValidationAndDelete(t *testing.T) {
	hosts := orchtest.NewFakeHosts("h1", "h2", "h3")
	o := startOne(t, orchtest.Options{NewHosts: func(inventory.Service) groups.Hosts { return hosts }})

	// A reader reads and may not write.
	_, err := o.reader.ApplyGroup(t.Context(), webGroup())
	require.Equal(t, 403, status(err))
	_, err = o.anon.Groups(t.Context())
	require.Equal(t, 401, status(err))

	bad := webGroup()
	bad.Region = ""
	_, err = o.op.ApplyGroup(t.Context(), bad)
	require.Equal(t, 400, status(err))

	_, err = o.op.Group(t.Context(), "web")
	require.Equal(t, 404, status(err))

	_, err = o.op.ApplyGroup(t.Context(), webGroup())
	require.NoError(t, err)
	gs, err := o.reader.Groups(t.Context())
	require.NoError(t, err)
	require.Len(t, gs, 1)
	require.Equal(t, "web", gs[0].Spec.Name)
	_, err = o.anon.MicroVMs(t.Context())
	require.Equal(t, 401, status(err))

	// A group with members cannot be deleted; once its count is 0 and they are gone it can.
	o.c.Leader(t, []int{0}, 30*time.Second)
	waitRunning(t, o.op, hosts, 3)
	vms, err := o.reader.MicroVMs(t.Context())
	require.NoError(t, err)
	require.Len(t, vms, 3)
	for _, v := range vms {
		require.Equal(t, "web", v.Group)
		require.Equal(t, "running", v.State)
	}
	require.Equal(t, 409, status(o.op.DeleteGroup(t.Context(), "web")))

	zero := webGroup()
	zero.Count = 0
	_, err = o.op.ApplyGroup(t.Context(), zero)
	require.NoError(t, err)
	// Forgetting a member is recorded in the store a pass after the host lost it.
	require.Eventually(t, func() bool {
		return len(hosts.Running()) == 0 && o.op.DeleteGroup(t.Context(), "web") == nil
	}, 30*time.Second, 100*time.Millisecond)
	require.Equal(t, 404, status(o.op.DeleteGroup(t.Context(), "web")))
}
