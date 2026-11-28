package orchd_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// reportedPrefixes is what the hosts say they hold: the Machine prefixes by host, and no entry for a
// host that does not answer.
type reportedPrefixes struct {
	mu sync.Mutex
	by map[string][]string
}

func (r *reportedPrefixes) set(host string, prefixes ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.by[host] = prefixes
}

func (r *reportedPrefixes) MachinePrefixes(context.Context) (map[string][]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]string{}
	for h, p := range r.by {
		out[h] = p
	}
	return out, nil
}

func TestAJoinedHostIsNeverGivenAPrefixAHostEnrolledWithItsOwnConfigurationHolds(t *testing.T) {
	reported := &reportedPrefixes{by: map[string][]string{}}
	o := startOne(t, orchtest.Options{NewPrefixHosts: func(inventory.Service) inventory.PrefixHosts { return reported }})
	require.NoError(t, o.op.SetProfile(t.Context(), profileYAML(t, "10.200.0.0/16")))

	// A control host chose its Machine prefix itself (naructl gen node --machine-prefix), and orch
	// recorded nothing when it admitted it.
	control := newHost(t, "node-1")
	_, err := o.anon.Enroll(t.Context(), control.request(o.keyOf(t, control.id)))
	require.NoError(t, err)
	o.admit(t, control)
	require.Empty(t, o.allocations(t), "orch recorded no prefix at the admission of a host that was not given one")

	joined := newHost(t, "h-0123456789abcdef")
	_, err = o.anon.Enroll(t.Context(), joined.seeded(o.keyOf(t, joined.id)))
	require.NoError(t, err)

	// The control host has not reported yet, so the pool could give out its prefix: the admission waits.
	_, err = o.tryAdmit(t, joined)
	require.Error(t, err)
	assert.Equal(t, 409, status(err), "the admission is refused until every admitted host's prefix is recorded")
	assert.Contains(t, err.Error(), "node-1")
	pending, err := o.op.Pending(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1, "the refused host stays pending")

	reported.set("node-1", "10.200.1.0/24")
	require.Eventually(t, func() bool { return o.allocations(t)["10.200.1.0/24"].HostID == "node-1" }, 10*time.Second, 50*time.Millisecond,
		"the leader records what the host reports")

	got, err := o.tryAdmit(t, joined)
	require.NoError(t, err)
	// The control: with the pool's first usable prefix, 10.200.1.0/24, held by node-1, the joined host
	// takes the next. TestAdmittingAHostFromTheSeedBuildsItsConfiguration... shows 10.200.1.0/24 is what
	// the same pool gives when nothing is held.
	assert.Equal(t, "10.200.2.0/24", got.MachinePrefix)
	assert.Equal(t, "node-1", o.allocations(t)["10.200.1.0/24"].HostID, "the host keeps the prefix it reported")
}

func TestAPrefixTwoHostsReportIsAnEventAndStaysWithTheFirst(t *testing.T) {
	reported := &reportedPrefixes{by: map[string][]string{}}
	o := startOne(t, orchtest.Options{NewPrefixHosts: func(inventory.Service) inventory.PrefixHosts { return reported }})
	for _, id := range []string{"node-1", "node-2"} {
		h := newHost(t, id)
		_, err := o.anon.Enroll(t.Context(), h.request(o.keyOf(t, h.id)))
		require.NoError(t, err)
		o.admit(t, h)
	}
	reported.set("node-1", "10.200.1.0/24")
	require.Eventually(t, func() bool { return o.allocations(t)["10.200.1.0/24"].HostID == "node-1" }, 10*time.Second, 50*time.Millisecond)

	reported.set("node-2", "10.200.1.0/24")
	require.Eventually(t, func() bool {
		events, err := o.op.Events(t.Context())
		require.NoError(t, err)
		for _, e := range events {
			if e.Type == inventory.EventPrefixConflict && e.Subject == "node-2" && e.Detail["prefix"] == "10.200.1.0/24" {
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond)
	assert.Equal(t, "node-1", o.allocations(t)["10.200.1.0/24"].HostID, "the first host keeps it")
}
