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

func TestApply_AnExtensionPatchMergesByDigestSoOneNameKeepsAnEntryPerVersion(t *testing.T) {
	r := newRig(t)
	old, next := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"extensions": []any{obj{"name": "tools", "sha256": old}}}), Mode: apply.Staged})
	require.NoError(t, err)

	// The new version's entry goes beside the old one.
	_, err = r.engine.Apply(apply.Request{Revision: 1, Patch: patch(obj{"extensions": []any{obj{"name": "tools", "sha256": next}}}), Mode: apply.Staged})
	require.NoError(t, err)
	_, cfg := r.engine.Config()
	assert.Equal(t, []config.Extension{{Name: "tools", SHA256: old}, {Name: "tools", SHA256: next}}, cfg.Spec.Extensions)

	// After the promotion the old entry is removed by its digest.
	_, err = r.engine.Apply(apply.Request{Revision: 2, Patch: patch(obj{"extensions": []any{obj{"sha256": old, "$patch": "delete"}}}), Mode: apply.Staged})
	require.NoError(t, err)
	_, cfg = r.engine.Config()
	assert.Equal(t, []config.Extension{{Name: "tools", SHA256: next}}, cfg.Spec.Extensions)
}

func TestApply_AnEntryWithNoDigestIsMergedAndDeletedByItsName(t *testing.T) {
	r := newRig(t)
	pinned := strings.Repeat("ab", 32)
	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"extensions": []any{obj{"name": "tools", "sha256": pinned}, obj{"name": "naru-orch"}, obj{"name": "csi"}}}), Mode: apply.Staged})
	require.NoError(t, err)

	// The same unpinned entry again is the same element, not a second one.
	_, err = r.engine.Apply(apply.Request{Revision: 1, Patch: patch(obj{"extensions": []any{obj{"name": "naru-orch"}}}), Mode: apply.Staged})
	require.NoError(t, err)
	_, cfg := r.engine.Config()
	assert.Equal(t, []config.Extension{{Name: "tools", SHA256: pinned}, {Name: "naru-orch"}, {Name: "csi"}}, cfg.Spec.Extensions)

	_, err = r.engine.Apply(apply.Request{Revision: 2, Patch: patch(obj{"extensions": []any{obj{"name": "csi", "$patch": "delete"}}}), Mode: apply.Staged})
	require.NoError(t, err)
	_, cfg = r.engine.Config()
	assert.Equal(t, []config.Extension{{Name: "tools", SHA256: pinned}, {Name: "naru-orch"}}, cfg.Spec.Extensions)
}

func TestApply_APinnedAndAnUnpinnedEntryOfOneNameAreTwoElements(t *testing.T) {
	r := newRig(t)
	pinned := strings.Repeat("ab", 32)
	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"extensions": []any{obj{"name": "tools", "sha256": pinned}}}), Mode: apply.Staged})
	require.NoError(t, err)

	_, err = r.engine.Apply(apply.Request{Revision: 1, Patch: patch(obj{"extensions": []any{obj{"name": "tools"}}}), Mode: apply.Staged})
	require.NoError(t, err)
	_, cfg := r.engine.Config()
	assert.Equal(t, []config.Extension{{Name: "tools", SHA256: pinned}, {Name: "tools"}}, cfg.Spec.Extensions, "the unpinned entry is appended, not merged into the pinned one")

	_, err = r.engine.Apply(apply.Request{Revision: 2, Patch: patch(obj{"extensions": []any{obj{"name": "tools", "$patch": "delete"}}}), Mode: apply.Staged})
	require.NoError(t, err)
	_, cfg = r.engine.Config()
	assert.Equal(t, []config.Extension{{Name: "tools", SHA256: pinned}}, cfg.Spec.Extensions, "deleting by name removes the unpinned entry only")
}

func TestApply_AnExtensionElementWithNeitherADigestNorANameIsRefusedAndTheMessageSaysBoth(t *testing.T) {
	r := newRig(t)

	_, err := r.engine.Apply(apply.Request{Patch: patch(obj{"extensions": []any{obj{"version": "x"}}}), Mode: apply.Staged})

	var invalid *apply.InvalidError
	require.ErrorAs(t, err, &invalid)
	assert.Contains(t, invalid.Message, "or its name where it has no sha256")
}
