package orchd_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchtest"
	"github.com/garamsh/naru/orch/internal/trust"
)

type trustHosts struct{ at time.Time }

func (h trustHosts) Trust(context.Context) ([]trust.Host, error) {
	return []trust.Host{{ID: "host-a", ExpiresAt: h.at}}, nil
}

// expiring returns the authority.expiring events by subject once both are there.
func expiring(t *testing.T, o *one, subjects ...string) map[string]orchapi.Event {
	t.Helper()
	got := map[string]orchapi.Event{}
	require.Eventually(t, func() bool {
		events, err := o.reader.Events(t.Context())
		require.NoError(t, err)
		for _, ev := range events {
			if ev.Type == trust.EventExpiring {
				got[ev.Subject] = ev
			}
		}
		return len(got) == len(subjects)
	}, 10*time.Second, 50*time.Millisecond)
	return got
}

func TestAMemberWarnsOfItsPoolAndTheLeaderOfAHostsWhenEitherEndsWithinAYear(t *testing.T) {
	at := time.Now().AddDate(0, 11, 0) // the cluster's clock starts at the current time
	o := startOne(t, orchtest.Options{TrustPools: true, NewTrustHosts: func(inventory.Service) trust.Hosts { return trustHosts{at: at} }})
	name := "orch/" + o.c.Members[0].Name()
	got := expiring(t, o, name, "host-a")
	member := got[name]
	assert.Equal(t, "ca", member.Detail["pool"], "the member's --ca ends within a day")
	assert.Equal(t, "client-ca", got["host-a"].Detail["pool"])
}
