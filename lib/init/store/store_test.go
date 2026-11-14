package store_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/store"
)

const doc = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  services:
    - name: mvmd
      binary: /usr/bin/mvmd
      args: ["--state-dir", "/var/lib/mvmd"]
      env:
        MVMD_LOG: debug
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/naru
  network:
    forwarding: []
  pki:
    authorities: []
    certificates: []
  files:
    - path: /run/mvmd/config.json
      mode: "0600"
      content: |
        {"listen": ":8080"}
      restart: [mvmd]
`

func parsed(t *testing.T) config.NodeConfig {
	t.Helper()
	cfg, err := config.Parse([]byte(doc))
	require.NoError(t, err)
	return cfg
}

func TestStore_LoadReturnsWhatSaveWrote(t *testing.T) {
	s := store.New(t.TempDir())
	want := store.Stored{Revision: 3, BaseSha256: store.Digest([]byte(doc)), Config: parsed(t)}

	require.NoError(t, s.Save(want))
	got, err := s.Load()

	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestStore_SaveReplacesAndLeavesNothingBeside(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config")
	s := store.New(dir)
	require.NoError(t, s.Save(store.Stored{Revision: 1, Config: parsed(t)}))
	require.NoError(t, s.Save(store.Stored{Revision: 2, Config: parsed(t)}))

	got, err := s.Load()

	require.NoError(t, err)
	assert.Equal(t, 2, got.Revision)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, store.FileName, entries[0].Name())
	info, err := entries[0].Info()
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the store holds secrets")
}

func TestStore_ASaveThatFailsLeavesThePreviousStore(t *testing.T) {
	dir := t.TempDir()
	s := store.New(dir)
	require.NoError(t, s.Save(store.Stored{Revision: 1, Config: parsed(t)}))
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		_ = f.Close()
		t.Skip("the directory is writable despite its mode, as for root")
	}

	err := s.Save(store.Stored{Revision: 2, Config: parsed(t)})

	require.Error(t, err)
	got, err := s.Load()
	require.NoError(t, err)
	assert.Equal(t, 1, got.Revision)
}

func TestStore_LoadWithoutAStoreIsNotFound(t *testing.T) {
	_, err := store.New(t.TempDir()).Load()

	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestStore_LoadRefusesWhatDoesNotValidate(t *testing.T) {
	tests := map[string]string{
		"not YAML":                   "revision: [",
		"an unknown field":           "revision: 1\nbogus: 2\nconfig: {}\n",
		"a configuration that fails": "revision: 1\nbaseSha256: x\nconfig:\n  apiVersion: naru.garam.sh/v1alpha1\n",
		"a negative revision":        "revision: -1\nconfig: {}\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, store.FileName), []byte(content), 0o600))

			_, err := store.New(dir).Load()

			require.Error(t, err)
			assert.NotErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestDigest_IsTheSHA256OfTheBytes(t *testing.T) {
	assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", store.Digest([]byte("abc")))
}
