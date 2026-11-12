package boot_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/store"
)

// storedWith returns a store of revision 3 whose configuration is validConfig
// with its service mvmd named stored and its data disk labelled OTHER, derived
// from the base raw unless digest says otherwise.
func storedWith(t *testing.T, raw string, digest string) *store.Stored {
	t.Helper()
	doc := replace(t, replace(t, validConfig, "name: mvmd", "name: stored"), "label: NARUDATA", "label: OTHER")
	cfg, err := config.Parse([]byte(doc))
	require.NoError(t, err)
	if digest == "" {
		digest = store.Digest([]byte(raw))
	}
	return &store.Stored{Revision: 3, BaseSha256: digest, Config: cfg}
}

func TestRun_UsesTheStoredConfigurationWhenItWasDerivedFromTheCurrentBase(t *testing.T) {
	r := &recorder{raw: validConfig}
	r.stored = storedWith(t, validConfig, "")

	got, err := runStored(r)

	require.NoError(t, err)
	assert.Equal(t, []string{"stored"}, r.started, "the stored configuration's services start")
	assert.Equal(t, 3, got.Revision)
	assert.Equal(t, "stored", got.Config.Spec.Services[0].Name)
	assert.Equal(t, "NARUDATA", r.mounted.Label, "the data disk is mounted from the base, before the store can be read")
	assert.Empty(t, r.saved, "nothing is stored over a store that is used")
	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "prepare", "seal", "start stored"}, r.calls)
}

func TestRun_DiscardsAStoredConfigurationWhoseBaseChangedAndStoresTheBaseAsRevisionZero(t *testing.T) {
	r := &recorder{raw: validConfig}
	r.stored = storedWith(t, validConfig, store.Digest([]byte("an older user-data")))

	got, err := runStored(r)

	require.NoError(t, err)
	assert.Equal(t, []string{"mvmd"}, r.started, "the base's services start")
	assert.Equal(t, 0, got.Revision)
	require.Len(t, r.saved, 1)
	assert.Equal(t, 0, r.saved[0].Revision)
	assert.Equal(t, store.Digest([]byte(validConfig)), r.saved[0].BaseSha256)
	assert.Equal(t, "mvmd", r.saved[0].Config.Spec.Services[0].Name)
}

func TestRun_DiscardsAStoredConfigurationThatDoesNotValidate(t *testing.T) {
	r := &recorder{raw: validConfig, loadErr: errors.New("store.yaml: spec.dataDisk: must name the disk")}
	r.stored = storedWith(t, validConfig, "")

	got, err := runStored(r)

	require.NoError(t, err)
	assert.Equal(t, []string{"mvmd"}, r.started)
	assert.Equal(t, 0, got.Revision)
	require.Len(t, r.saved, 1)
	assert.Equal(t, 0, r.saved[0].Revision)
}

func TestRun_RunsTheBaseWhenThereIsNoStore(t *testing.T) {
	r := &recorder{raw: validConfig}

	got, err := runStored(r)

	require.NoError(t, err)
	assert.Equal(t, []string{"mvmd"}, r.started)
	assert.Equal(t, 0, got.Revision)
	assert.Equal(t, store.Digest([]byte(validConfig)), got.BaseSha256)
	assert.Empty(t, r.saved, "a Node that never applied a change has nothing to store")
}
