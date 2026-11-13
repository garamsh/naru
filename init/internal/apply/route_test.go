package apply_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/apply"
)

func route(to, via string) obj { return obj{"to": to, "via": via} }

func TestApply_SpecNetworkRoutesAreLiveAndPlannedByDestination(t *testing.T) {
	store := apply.Action{Action: "store", Target: "config"}
	two := patch(obj{"network": obj{"routes": []any{route("10.200.2.0/24", "10.78.0.2")}}})
	tests := []struct {
		name    string
		before  obj // applied first, to give the running configuration its routes
		patch   obj
		calls   []string
		actions []apply.Action
	}{
		{
			name:    "added",
			patch:   two,
			calls:   []string{"store", "add-route 10.200.2.0/24 via 10.78.0.2"},
			actions: []apply.Action{store, {Action: "add-route", Target: "10.200.2.0/24"}},
		},
		{
			name:    "a second one beside the first",
			before:  two,
			patch:   patch(obj{"network": obj{"routes": []any{route("10.200.3.0/24", "10.78.0.3")}}}),
			calls:   []string{"store", "add-route 10.200.3.0/24 via 10.78.0.3"},
			actions: []apply.Action{store, {Action: "add-route", Target: "10.200.3.0/24"}},
		},
		{
			name:    "a new next hop for a destination replaces its route",
			before:  two,
			patch:   patch(obj{"network": obj{"routes": []any{route("10.200.2.0/24", "10.78.0.9")}}}),
			calls:   []string{"store", "add-route 10.200.2.0/24 via 10.78.0.9"},
			actions: []apply.Action{store, {Action: "add-route", Target: "10.200.2.0/24"}},
		},
		{
			name:    "deleted",
			before:  two,
			patch:   patch(obj{"network": obj{"routes": []any{obj{"to": "10.200.2.0/24", "$patch": "delete"}}}}),
			calls:   []string{"store", "delete-route 10.200.2.0/24 via 10.78.0.2"},
			actions: []apply.Action{store, {Action: "delete-route", Target: "10.200.2.0/24"}},
		},
		{
			name:    "unchanged",
			before:  two,
			patch:   patch(obj{"network": obj{"routes": []any{route("10.200.2.0/24", "10.78.0.2")}}, "services": []any{obj{"name": "b", "args": []any{"q"}}}}),
			calls:   []string{"store", "stop b", "start b"},
			actions: []apply.Action{store, {Action: "restart", Target: "b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			if tt.before != nil {
				rev, _ := r.engine.Config()
				_, err := r.engine.Apply(apply.Request{Revision: rev, Patch: tt.before, Mode: apply.NoReboot})
				require.NoError(t, err)
				r.mu.Lock()
				r.journal = nil
				r.mu.Unlock()
			}
			rev, _ := r.engine.Config()

			res, err := r.engine.Apply(apply.Request{Revision: rev, Patch: tt.patch, Mode: apply.NoReboot})

			require.NoError(t, err)
			assert.Equal(t, tt.calls, r.calls())
			assert.Equal(t, tt.actions, res.Actions)
		})
	}
}

func TestApply_AFailedRouteIsAFailedActionAndTheRevisionStays(t *testing.T) {
	r := newRig(t)
	r.failOn = "add-route 10.200.2.0/24 via 10.78.0.2"

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"network": obj{"routes": []any{route("10.200.2.0/24", "10.78.0.2")}}}), Mode: apply.NoReboot})

	var failed *apply.FailedError
	require.ErrorAs(t, err, &failed)
	assert.Equal(t, 1, failed.Revision)
	assert.Equal(t, apply.Action{Action: "add-route", Target: "10.200.2.0/24"}, failed.Failed)
}

func TestApply_StagedSpecNetworkRoutesTakeNoLiveAction(t *testing.T) {
	r := newRig(t)

	r.staged(t, patch(obj{"network": obj{"routes": []any{route("10.200.2.0/24", "10.78.0.2")}}}))

	assert.Equal(t, []string{"store"}, r.calls())
}
