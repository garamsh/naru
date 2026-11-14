package orchd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/orchtest"
)

// failover is the bound the test holds the takeover to: the 3 s lease plus the election.
const failover = 20 * time.Second

func TestThreeMembersElectOneLeaderAndAnotherTakesOver(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 3})

	all := []int{0, 1, 2}
	first := c.Leader(t, all, 30*time.Second)
	t.Logf("leader: m%d", first)

	// Exactly one member leads, and every member agrees who it is.
	for _, i := range all {
		name, err := c.Members[i].Leader(t.Context())
		require.NoError(t, err)
		require.Equal(t, c.Members[first].Name(), name)
	}

	// Kill the leader: its etcd member and its API stop, and its lease lapses.
	require.NoError(t, c.Members[first].Stop())
	var live []int
	for _, i := range all {
		if i != first {
			live = append(live, i)
		}
	}
	start := time.Now()
	second := c.Leader(t, live, failover)
	t.Logf("old leader: m%d, new leader: m%d, takeover took %v", first, second, time.Since(start))
	require.NotEqual(t, first, second)
	require.False(t, c.Members[first].IsLeader())
}
