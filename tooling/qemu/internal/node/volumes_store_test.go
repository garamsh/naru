package node

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func volumesImageDir(t *testing.T, image, info string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "extensions"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "extensions", "naru-volumes.sqfs"), []byte(image), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "extensions", "naru-volumes.json"), []byte(info), 0o600))
	return dir
}

func TestStoreVolumesExtension_StoresTheImageUnderItsDigestWithAManifestThatNamesNoService(t *testing.T) {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("squashfs")))
	dir := volumesImageDir(t, "squashfs", `{"name":"naru-volumes","version":"v9","sha256":"`+sum+`","services":[]}`)
	store := filepath.Join(t.TempDir(), "store")

	require.NoError(t, StoreVolumesExtension(store, dir))

	image, err := os.ReadFile(filepath.Join(store, sum+".sqfs"))
	require.NoError(t, err)
	assert.Equal(t, "squashfs", string(image))
	manifest, err := os.ReadFile(filepath.Join(store, sum+".json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"naru-volumes","version":"v9","services":[]}`, string(manifest))
	var m map[string]any
	require.NoError(t, json.Unmarshal(manifest, &m))
	assert.NotNil(t, m["services"], "an empty list, not null")
}

func TestStoreVolumesExtension_RefusesADescriptionThatDisagreesWithTheImageOrNamesAnotherExtension(t *testing.T) {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("squashfs")))
	bad := fmt.Sprintf("%x", sha256.Sum256([]byte("other")))
	for name, info := range map[string]string{
		"another digest":   `{"name":"naru-volumes","version":"v9","sha256":"` + bad + `","services":[]}`,
		"another name":     `{"name":"naru-orch","version":"v9","sha256":"` + sum + `","services":[]}`,
		"a service":        `{"name":"naru-volumes","version":"v9","sha256":"` + sum + `","services":["x"]}`,
		"no version":       `{"name":"naru-volumes","sha256":"` + sum + `","services":[]}`,
		"a malformed hash": `{"name":"naru-volumes","version":"v9","sha256":"zz","services":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := StoreVolumesExtension(t.TempDir(), volumesImageDir(t, "squashfs", info))
			assert.Error(t, err)
		})
	}
}
