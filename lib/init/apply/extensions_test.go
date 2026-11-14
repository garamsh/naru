package apply_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/config"
)

func TestApply_AnExtensionChangeIsRefusedInNoRebootAndTakenAtTheNextBoot(t *testing.T) {
	r := newRig(t)
	digest := strings.Repeat("ab", 32)
	add := patch(obj{"extensions": []any{obj{"name": "tools", "sha256": digest}}})

	_, err := r.engine.Apply(apply.Request{Patch: add, Mode: apply.NoReboot})

	var invalid *apply.InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Equal(t, []string{"spec.extensions"}, invalid.Fields)
	assert.Empty(t, r.calls(), "nothing stored")

	_, err = r.engine.Apply(apply.Request{Patch: add, Mode: apply.Staged})

	require.NoError(t, err)
	assert.Equal(t, []string{"store"}, r.calls(), "stored for the next boot, no live action")
	require.Len(t, r.saved, 1)
	assert.Equal(t, []config.Extension{{Name: "tools", SHA256: digest}}, r.saved[0].Config.Spec.Extensions)
}

func TestApply_AnExtensionPatchMergesByName(t *testing.T) {
	r := newRig(t)
	first, second := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"extensions": []any{
		obj{"name": "a", "sha256": first}, obj{"name": "b", "sha256": second},
	}}), Mode: apply.Staged})
	require.NoError(t, err)

	_, err = r.engine.Apply(apply.Request{Revision: 1, Patch: patch(obj{"extensions": []any{
		obj{"name": "a", "sha256": second}, obj{"name": "b", "$patch": "delete"},
	}}), Mode: apply.Staged})

	require.NoError(t, err)
	_, cfg := r.engine.Config()
	assert.Equal(t, []config.Extension{{Name: "a", SHA256: second}}, cfg.Spec.Extensions)
}
