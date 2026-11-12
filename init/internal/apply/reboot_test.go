package apply_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/apply"
)

func TestApply_RebootStoresARebootOnlyFieldTakesNoLiveActionAndAsksTheCallerToReboot(t *testing.T) {
	r := newRig(t)

	res, err := r.engine.Apply(apply.Request{Patch: patch(obj{"network": obj{"forwarding": []any{"ipv4", "ipv6"}}, "services": []any{obj{"name": "a", "args": []any{"-y"}}}}), Mode: apply.Reboot})

	require.NoError(t, err)
	assert.Equal(t, apply.Result{Revision: 1, Mode: apply.Reboot, Reboot: true,
		Actions: []apply.Action{{Action: "store", Target: "config"}, {Action: "reboot", Target: "node"}}}, res)
	assert.Equal(t, []string{"store"}, r.calls(), "the change reaches the next boot and the services are not touched")
	require.Len(t, r.saved, 1)
	assert.Len(t, r.saved[0].Config.Spec.Network.Forwarding, 2)
	rev, _ := r.engine.Config()
	assert.Equal(t, 1, rev)
}

func TestApply_RebootRefusesWhatStagedRefusesAndAsksForNoReboot(t *testing.T) {
	tests := []struct {
		name string
		req  apply.Request
	}{
		{"a never field", apply.Request{Patch: patch(obj{"dataDisk": obj{"mountPoint": "/mnt"}}), Mode: apply.Reboot}},
		{"a stale revision", apply.Request{Revision: 2, Patch: patch(obj{"controlPlane": true}), Mode: apply.Reboot}},
		{"a result that does not validate", apply.Request{Patch: patch(obj{"services": []any{obj{"name": "a", "binary": "relative"}}}), Mode: apply.Reboot}},
	}
	control := newRig(t)
	_, err := control.engine.Apply(apply.Request{Patch: patch(obj{"controlPlane": true}), Mode: apply.Reboot})
	require.NoError(t, err, "the control: a valid change is accepted in the same mode")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)

			res, err := r.engine.Apply(tt.req)

			require.Error(t, err)
			assert.Zero(t, res, "the result carries no reboot")
			assert.Empty(t, r.calls(), "nothing stored, nothing live")
		})
	}
}

func TestApply_ARebootModeDryRunListsTheRebootAndDoesNotAskForOne(t *testing.T) {
	r := newRig(t)

	res, err := r.engine.Apply(apply.Request{Patch: patch(obj{"controlPlane": true}), Mode: apply.Reboot, DryRun: true})

	require.NoError(t, err)
	assert.Equal(t, apply.Result{Revision: 0, Mode: apply.Reboot, DryRun: true,
		Actions: []apply.Action{{Action: "store", Target: "config"}, {Action: "reboot", Target: "node"}}}, res)
	assert.Empty(t, r.calls())
}

func TestApply_AFailedStoreInRebootModeAsksForNoReboot(t *testing.T) {
	r := newRig(t)
	r.failOn = "store"

	res, err := r.engine.Apply(apply.Request{Patch: patch(obj{"controlPlane": true}), Mode: apply.Reboot})

	var stored *apply.StoreError
	require.ErrorAs(t, err, &stored)
	assert.False(t, res.Reboot)
}

func TestApply_OnlyRebootModeAsksForAReboot(t *testing.T) {
	for _, mode := range []apply.Mode{apply.NoReboot, apply.Staged} {
		t.Run(string(mode), func(t *testing.T) {
			r := newRig(t)

			res, err := r.engine.Apply(apply.Request{Patch: patch(obj{"services": []any{obj{"name": "a", "args": []any{"-y"}}}}), Mode: mode})

			require.NoError(t, err)
			assert.False(t, res.Reboot)
			assert.NotContains(t, res.Actions, apply.Action{Action: "reboot", Target: "node"})
		})
	}
}
