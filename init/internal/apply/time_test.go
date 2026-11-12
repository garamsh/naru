package apply_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/apply"
)

func TestApply_SpecTimeIsPlannedFirstAndOnlyWhenItChanged(t *testing.T) {
	store := apply.Action{Action: "store", Target: "config"}
	tests := []struct {
		name    string
		before  obj // applied first, to give the running configuration its spec.time
		patch   obj
		calls   []string
		actions []apply.Action
	}{
		{
			name:    "added",
			patch:   patch(obj{"time": obj{"servers": []any{"a.example", "b.example:1123"}}, "files": []any{obj{"path": "/run/f2", "content": "new"}}}),
			calls:   []string{"store", "time [a.example:123 b.example:1123]", "file /run/f2"},
			actions: []apply.Action{store, {Action: "sync-time", Target: "spec.time"}, {Action: "write-file", Target: "/run/f2"}},
		},
		{
			name:    "changed",
			before:  patch(obj{"time": obj{"servers": []any{"a.example"}}}),
			patch:   patch(obj{"time": obj{"servers": []any{"c.example"}}}),
			calls:   []string{"store", "time [c.example:123]"},
			actions: []apply.Action{store, {Action: "sync-time", Target: "spec.time"}},
		},
		{
			name:    "removed",
			before:  patch(obj{"time": obj{"servers": []any{"a.example"}}}),
			patch:   patch(obj{"time": nil}),
			calls:   []string{"store", "time []"},
			actions: []apply.Action{store, {Action: "stop-time-sync", Target: "spec.time"}},
		},
		{
			name:    "the same server written with the default port",
			before:  patch(obj{"time": obj{"servers": []any{"a.example"}}}),
			patch:   patch(obj{"time": obj{"servers": []any{"a.example:123"}}}),
			calls:   []string{"store"},
			actions: []apply.Action{store},
		},
		{
			name:    "unchanged",
			before:  patch(obj{"time": obj{"servers": []any{"a.example"}}}),
			patch:   patch(obj{"services": []any{obj{"name": "b", "args": []any{"q"}}}}),
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

func TestApply_StagedSpecTimeTakesNoLiveAction(t *testing.T) {
	r := newRig(t)

	r.staged(t, patch(obj{"time": obj{"servers": []any{"a.example"}}}))

	assert.Equal(t, []string{"store"}, r.calls())
}
