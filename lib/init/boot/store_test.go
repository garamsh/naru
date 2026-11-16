package boot_test

import (
	"errors"
	"fmt"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/boot"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/wire"
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
	assert.Equal(t, []string{"read", "network", "mount", "load", "api", "forward", "files", "registries", "prepare", "seal", "start stored"}, r.calls)
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

func TestRun_KeepsAStoreThatCannotBeReadAndRunsTheBaseForThisBootOnly(t *testing.T) {
	r := &recorder{raw: validConfig, loadErr: fmt.Errorf("%w: %w", store.ErrUnreadable, syscall.EIO)}
	r.stored = storedWith(t, validConfig, "")

	got, err := runStored(r)

	require.NoError(t, err)
	assert.Equal(t, []string{"mvmd"}, r.started, "the base's services start")
	assert.Equal(t, 0, got.Revision)
	assert.Empty(t, r.saved, "the operator's revisions are not written over")
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

// seedless is a recorder with no NoCloud drive, which holds the stored configuration and network.
func seedless(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{readErr: fmt.Errorf("NoCloud drive \"cidata\": %w", boot.ErrNoSeed)}
	r.stored = storedWith(t, validConfig, "")
	r.storedNetwork, r.hasNetwork = []byte(validNetwork), true
	return r
}

func TestRun_WithNoSeedRunsTheStoredConfigurationOnTheStoredNetworkAndNeverFormats(t *testing.T) {
	r := seedless(t)

	got, err := runStored(r)

	require.NoError(t, err)
	assert.Equal(t, []string{"stored"}, r.started)
	assert.Equal(t, 3, got.Revision)
	assert.Equal(t, config.DataDisk{Label: wire.DataLabel, MountPoint: wire.DataMount}, r.mounted, "the data disk is found by the default label")
	require.Len(t, r.applied.Interfaces, 1)
	assert.Equal(t, "eth0", r.applied.Interfaces[0].Name, "the network is the stored one")
	assert.Equal(t, []string{"read", "mount", "load", "network", "api", "forward", "files", "registries", "prepare", "seal", "start stored"}, r.calls, "the disk is mounted before the network, which is stored on it, and never formatted")
	assert.Empty(t, r.saved)
	assert.False(t, r.savedNetwork)
}

func TestRun_WithNoSeedAndNoStoredConfigurationIsRefusedNamingBoth(t *testing.T) {
	r := seedless(t)
	r.stored = nil

	_, err := runStored(r)

	require.Error(t, err)
	assert.ErrorIs(t, err, boot.ErrNoSeed)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Empty(t, r.started)
	// The control: the same Node with its configuration stored boots.
	ok := seedless(t)
	_, err = runStored(ok)
	require.NoError(t, err)
}

func TestRun_WithNoSeedAndAStoredConfigurationThatDoesNotValidateIsRefused(t *testing.T) {
	r := seedless(t)
	r.loadErr = errors.New("store.yaml: spec.dataDisk: must name the disk")

	_, err := runStored(r)

	require.Error(t, err)
	assert.ErrorIs(t, err, boot.ErrNoSeed)
	assert.Contains(t, err.Error(), "must name the disk")
	assert.Empty(t, r.started)
	assert.Empty(t, r.saved, "a stored configuration is never replaced by a guess")
}

func TestRun_WithNoSeedAndNoStoredNetworkIsRefusedAndSaysToAttachTheSeed(t *testing.T) {
	r := seedless(t)
	r.hasNetwork = false

	_, err := runStored(r)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no stored network-config; attach the seed for one boot")
	assert.Empty(t, r.started)
	assert.NotContains(t, r.calls, "network", "no network is guessed")
}

func TestRun_WithNoSeedAndADataDiskThatCannotBeMountedIsRefused(t *testing.T) {
	r := seedless(t)
	r.diskErr = errors.New("no disk with that label")

	_, err := runStored(r)

	require.Error(t, err)
	assert.ErrorIs(t, err, boot.ErrNoSeed)
	assert.Contains(t, err.Error(), "no disk with that label")
	assert.Empty(t, r.started)
}

func TestRun_ASeedStoresItsNetworkConfigForTheNextBootWithNoSeed(t *testing.T) {
	r := &recorder{raw: validConfig}

	_, err := runStored(r)

	require.NoError(t, err)
	assert.True(t, r.savedNetwork)
	assert.Equal(t, validNetwork, string(r.networkSaved))
}

func TestRun_ASeedAttachedAgainWinsOverTheStoredConfigurationUnderTheDigestRule(t *testing.T) {
	r := &recorder{raw: validConfig}
	r.stored = storedWith(t, validConfig, store.Digest([]byte("an older user-data")))
	r.storedNetwork, r.hasNetwork = []byte("stale"), true

	got, err := runStored(r)

	require.NoError(t, err)
	assert.Equal(t, 0, got.Revision, "the seed's base replaces a stored configuration of another base")
	assert.Equal(t, validNetwork, string(r.networkSaved), "the stored network is the seed's again")
}
