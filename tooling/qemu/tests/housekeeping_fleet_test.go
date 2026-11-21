//go:build qemu_real

package tests_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests of orch's etcd housekeeping (ADR 0088) run three control Nodes whose orch members after the first
// join Node 1's (spec.orch.join), as an image starts a cluster that grows, with the leader's departure time
// and pass interval short in spec.orch.departAfter and spec.orch.housekeepingInterval.

const (
	removedMember = "removed a member that stayed unreachable"
	discardedDir  = "discarded the member directory of a member the cluster removed"
)

func TestFleet_TheLeaderRemovesAMemberThatStaysOutOnlyWhileTheOthersAnswerAndItRejoinsEmptyWhenItStarts(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 3, orchJoin: true, orchExtra: func(int) map[string]any {
		return map[string]any{"departAfter": "90s", "housekeepingInterval": "5s"}
	}})
	f.startOrch()
	var leader int
	eventually(t, "a member leads", func() bool { leader = f.leader(f.controls()); return leader != 0 })
	out := leader%3 + 1
	var alive []int
	for _, i := range f.controls() {
		if i != out {
			alive = append(alive, i)
		}
	}

	f.node(out).Stop()

	// The control: a member that has just stopped is not removed, and the two that answer keep the quorum.
	f.must(f.orchAs(f.root, leader, "enroll-key", "create"), "the two members left take a write")
	assert.False(t, f.node(leader).Has(removedMember), "a member is not removed before the departure time")
	// After it, the leader removes that member, and no other: the others answer, and the cluster had three.
	eventually(t, "the leader removes the member that stayed out", func() bool {
		return f.node(leader).Has(removedMember, orchName(out))
	})
	for _, i := range alive {
		assert.False(t, f.node(leader).Has(removedMember, orchName(i)), "orch-%d answers and is not removed", i)
	}

	// The member starts again with its old data directory, which the cluster has forgotten: it wipes it and joins afresh.
	require.NoError(t, f.node(out).Reboot())
	eventually(t, "the removed member discards its member directory", func() bool { return f.node(out).Has(discardedDir) })
	eventually(t, "the member serves again as a member of the cluster", func() bool {
		return f.orchAs(f.root, out, "host", "pending").Err == nil
	})
	f.must(f.orchAs(f.root, out, "enroll-key", "create"), "the rejoined member takes a write through the cluster")
}
