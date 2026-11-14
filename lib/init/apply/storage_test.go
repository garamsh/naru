package apply_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/config"
)

func TestApply_AStorageChangeIsRefusedInNoRebootAndTakenAtTheNextBoot(t *testing.T) {
	r := newRig(t)
	add := patch(obj{"storage": []any{obj{"name": "vmdisks", "label": "vmdisks"}}})

	_, err := r.engine.Apply(apply.Request{Patch: add, Mode: apply.NoReboot})

	var invalid *apply.InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, []string{"spec.storage"}, invalid.Fields)
	assert.Empty(t, r.calls(), "nothing stored")

	res, err := r.engine.Apply(apply.Request{Patch: add, Mode: apply.Staged})

	require.NoError(t, err)
	assert.Equal(t, 1, res.Revision)
	assert.Equal(t, []string{"store"}, r.calls(), "stored for the next boot, no live action")
	require.Len(t, r.saved, 1)
	assert.Equal(t, []config.Storage{{Name: "vmdisks", Label: "vmdisks"}}, r.saved[0].Config.Spec.Storage)
}

func TestApply_AStoragePatchMergesByName(t *testing.T) {
	r := newRig(t)
	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"storage": []any{
		obj{"name": "a", "label": "a"}, obj{"name": "b", "device": "/dev/sdc"},
	}}), Mode: apply.Staged})
	require.NoError(t, err)

	_, err = r.engine.Apply(apply.Request{Revision: 1, Patch: patch(obj{"storage": []any{
		obj{"name": "a", "mountPoint": "/mnt/a"}, obj{"name": "b", "$patch": "delete"},
	}}), Mode: apply.Staged})

	require.NoError(t, err)
	_, cfg := r.engine.Config()
	assert.Equal(t, []config.Storage{{Name: "a", Label: "a", MountPoint: "/mnt/a"}}, cfg.Spec.Storage)
}
