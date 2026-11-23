package run

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/upgrade"
)

func TestWriteInitStatus_CarriesTheVersionTheSlotAndTheLastUpgradeWhenThereIsOne(t *testing.T) {
	state := t.TempDir()
	u := upgrade.NewWith(upgrade.Config{Version: "v0.1.0-dev.14", StateDir: state})
	path := filepath.Join(t.TempDir(), "run", "naru-hostd", "init.json")

	require.NoError(t, writeInitStatus(path, "v0.1.0-dev.14", u))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":"v0.1.0-dev.14","slot":"default"}`, string(got), "no upgrade has ended, so there is no lastUpgrade")

	// The control: once an upgrade has ended, the same call carries it.
	require.NoError(t, os.WriteFile(filepath.Join(state, "last.json"),
		[]byte(`{"from":"v0.1.0-dev.13","to":"v0.1.0-dev.14","result":"rolled back","reason":"a service stopped","time":"2026-11-23T10:00:00Z"}`), 0o600))
	require.NoError(t, writeInitStatus(path, "v0.1.0-dev.14", u))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":"v0.1.0-dev.14","slot":"default","lastUpgrade":{"from":"v0.1.0-dev.13","to":"v0.1.0-dev.14","result":"rolled back","reason":"a service stopped","time":"2026-11-23T10:00:00Z"}}`, string(got))
}
