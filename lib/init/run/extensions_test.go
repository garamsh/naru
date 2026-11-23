package run

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/extension"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/upgrade"
)

// imageMounter stands for the kernel: an "image" here is the text of its manifest.json.
type imageMounter struct{}

func (imageMounter) Mount(image *os.File, dir string) error {
	data, err := io.ReadAll(io.NewSectionReader(image, 0, 1<<20))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, extension.ManifestPath), data, 0o644)
}

func (imageMounter) Unmount(dir string) error {
	return os.Remove(filepath.Join(dir, extension.ManifestPath))
}

const prunableDoc = `apiVersion: naru.garam.sh/v1alpha1
kind: NodeConfig
metadata:
  name: node-1
spec:
  dataDisk:
    label: NARUDATA
    mountPoint: /var/lib/naru
`

func manifestOf(name, version string) string {
	return `{"name":"` + name + `","version":"` + version + `","services":["x"]}`
}

func putImage(t *testing.T, s *extension.Store, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	_, err := s.Put(strings.NewReader(content), digest)
	require.NoError(t, err)
	return digest
}

func TestPruneExtensions_RemovesWhatNoPinnedEntryNamesAndLogsWhenTheConfigurationCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	images := extension.NewStore(filepath.Join(dir, "extensions"), filepath.Join(dir, "probe"), 1<<20, imageMounter{})
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	manager := extension.NewManager(images, imageMounter{}, filepath.Join(dir, "mnt"), "v2", log)
	pinned := putImage(t, images, manifestOf("tools", "v1"))
	stale := putImage(t, images, manifestOf("naru-orch", "v1"))
	configs := store.New(filepath.Join(dir, "config"))
	cfg, err := config.Parse([]byte(prunableDoc))
	require.NoError(t, err)
	cfg.Spec.Extensions = []config.Extension{{Name: "tools", SHA256: pinned}, {Name: "naru-orch"}}
	require.NoError(t, configs.Save(store.Stored{Config: cfg}))

	pruneExtensions(configs, manager, log)

	left, err := manager.List()
	require.NoError(t, err)
	require.Len(t, left, 1, "the control: with a readable configuration the unpinned image of another version is removed")
	assert.Equal(t, pinned, left[0].SHA256)
	assert.NotContains(t, logs.String(), "level=ERROR")
	assert.NotEqual(t, stale, left[0].SHA256)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "config", store.FileName), []byte("not: [valid"), 0o600))
	pruneExtensions(configs, manager, log)

	assert.Contains(t, logs.String(), "level=ERROR", "an unreadable configuration is logged, not dropped")
	assert.Contains(t, logs.String(), "read the configuration to remove the extension images")
}

func TestStoredExtensions_AHostWithNoStoredRevisionNamesTheExtensionsItsBaseRuns(t *testing.T) {
	dir := t.TempDir()
	images := extension.NewStore(filepath.Join(dir, "extensions"), filepath.Join(dir, "probe"), 1<<20, imageMounter{})
	manager := extension.NewManager(images, imageMounter{}, filepath.Join(dir, "mnt"), "v1", slog.New(slog.NewTextHandler(io.Discard, nil)))
	pinned := putImage(t, images, manifestOf("naru-orch", "v1"))
	configs := store.New(filepath.Join(dir, "config"))
	base, err := config.Parse([]byte(prunableDoc))
	require.NoError(t, err)
	base.Spec.Extensions = []config.Extension{{Name: "naru-orch", SHA256: pinned}}
	ran := func() *config.NodeConfig { return &base }

	entries, stored, err := storedExtensions(configs, manager, ran)

	require.NoError(t, err)
	assert.Equal(t, []upgrade.Entry{{Name: "naru-orch", SHA256: pinned}}, entries, "a revision-0 digest-pinned entry is counted")
	assert.Len(t, stored, 1)

	stored0 := base
	stored0.Spec.Extensions = []config.Extension{{Name: "tools"}}
	require.NoError(t, configs.Save(store.Stored{Revision: 1, Config: stored0}))
	entries, _, err = storedExtensions(configs, manager, ran)

	require.NoError(t, err)
	assert.Equal(t, []upgrade.Entry{{Name: "tools"}}, entries, "the control: a stored revision is what the next boot runs, and wins")
}
