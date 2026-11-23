package run

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
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

func TestWriteInitStatus_ACleanBootAfterAnUpgradeEndedStillCarriesItsOutcome(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(t.TempDir(), "init.json")
	outcome := `{"from":"v0.1.0-dev.13","to":"v0.1.0-dev.14","result":"promoted","time":"2026-11-23T10:00:00Z"}`
	// The boot that ended the upgrade recorded it in the state directory.
	require.NoError(t, os.WriteFile(filepath.Join(state, "last.json"), []byte(outcome), 0o600))

	// The next boot has a new Upgrader, finds nothing unfinished and writes the status once the data
	// disk, and so the state directory, is there.
	reboot := upgrade.NewWith(upgrade.Config{Version: "v0.1.0-dev.14", StateDir: state})
	require.NoError(t, writeInitStatus(path, "v0.1.0-dev.14", reboot))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":"v0.1.0-dev.14","slot":"default","lastUpgrade":`+outcome+`}`, string(got))

	// The control: written before the state directory is mounted, the outcome is missing.
	unmounted := upgrade.NewWith(upgrade.Config{Version: "v0.1.0-dev.14", StateDir: filepath.Join(t.TempDir(), "absent")})
	require.NoError(t, writeInitStatus(path, "v0.1.0-dev.14", unmounted))
	got, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"version":"v0.1.0-dev.14","slot":"default"}`, string(got))
}

func TestWriteInitStatus_IsTheBodyOfHostapiNaruInfo(t *testing.T) {
	state := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(state, "last.json"),
		[]byte(`{"from":"a","to":"b","result":"rolled back","reason":"r","time":"2026-11-23T10:00:00Z"}`), 0o600))
	path := filepath.Join(t.TempDir(), "init.json")
	require.NoError(t, writeInitStatus(path, "b", upgrade.NewWith(upgrade.Config{Version: "b", StateDir: state})))
	got, err := os.ReadFile(path)
	require.NoError(t, err)

	var info hostapi.NaruInfo
	dec := json.NewDecoder(bytes.NewReader(got))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&info), "every field init writes is a field of the API type")
	require.NotNil(t, info.LastUpgrade)
	assert.Equal(t, hostapi.Upgrade{From: "a", To: "b", Result: "rolled back", Reason: "r", Time: "2026-11-23T10:00:00Z"}, *info.LastUpgrade)
}
