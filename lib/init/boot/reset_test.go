package boot_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/store"
)

func TestRun_AResetOfScopeStoreDiscardsTheStoreAndTheBaseRunsAsRevisionZero(t *testing.T) {
	r := &recorder{raw: validConfig, resetScope: store.ScopeStore, resetWaiting: true}
	r.stored = storedWith(t, validConfig, "")
	r.stored.Revision = 5

	_, err := runStored(r)

	require.NoError(t, err)
	assert.True(t, r.discarded)
	assert.False(t, r.cleared)
	assert.Nil(t, r.wiped)
	// The control: with no reset waiting nothing is discarded and the stored revision runs.
	ok := &recorder{raw: validConfig}
	ok.stored = storedWith(t, validConfig, store.Digest([]byte(validConfig)))
	ok.stored.Revision = 5
	got, err := runStored(ok)
	require.NoError(t, err)
	assert.False(t, ok.discarded)
	assert.Equal(t, 5, got.Revision)
}

func TestRun_AResetIsCarriedOutAfterTheDiskIsMountedAndBeforeTheStoreIsRead(t *testing.T) {
	r := &recorder{raw: validConfig, resetScope: store.ScopeStore, resetWaiting: true}

	_, err := runStored(r)

	require.NoError(t, err)
	assert.Equal(t, []string{"network", "mount", "discard", "load"}, r.calls[1:5])
}

func TestRun_AResetOfScopeDataClearsTheDataDiskAndKeepsTheStore(t *testing.T) {
	r := &recorder{raw: validConfig, resetScope: store.ScopeData, resetWaiting: true}

	_, err := runStored(r)

	require.NoError(t, err)
	assert.True(t, r.cleared)
	assert.False(t, r.discarded)
	assert.Nil(t, r.wiped)
}

func TestRun_AResetOfScopeAllBlanksTheDiskFormatsItAndMountsItAgain(t *testing.T) {
	r := &recorder{raw: validConfig, resetScope: store.ScopeAll, resetWaiting: true}

	_, err := runStored(r)

	require.NoError(t, err)
	require.NotNil(t, r.wiped)
	assert.Equal(t, "/var/lib/mvmd", r.wiped.MountPoint)
	assert.Equal(t, []string{"network", "mount", "wipe", "format", "mount", "load"}, r.calls[1:7], "blanked, then formatted as a blank disk is, then mounted")
	assert.Equal(t, *r.wiped, r.formatted)
}

func TestRun_AResetThatFailsIsARefusalAndNothingStarts(t *testing.T) {
	tests := map[string]*recorder{
		"clear":   {raw: validConfig, resetScope: store.ScopeData, resetWaiting: true, clearErr: errors.New("busy")},
		"wipe":    {raw: validConfig, resetScope: store.ScopeAll, resetWaiting: true, wipeErr: errors.New("busy")},
		"format":  {raw: validConfig, resetScope: store.ScopeAll, resetWaiting: true, formatErr: errors.New("no blank disk")},
		"request": {raw: validConfig, resetWaiting: true, resetErr: errors.New("reset.json: unknown scope")},
	}
	for name, r := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := runStored(r)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "reset")
			assert.Empty(t, r.started)
		})
	}
}
