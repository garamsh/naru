package run

import (
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

	"github.com/garamsh/naru/lib/init/apply"
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

// specsOf is the source run gives pruneExtensions and nextBootExtensions: the configuration an
// engine holds, or none where the boot made no engine.
func specsOf(e *apply.Engine) func() ([]config.Extension, bool) {
	return func() ([]config.Extension, bool) {
		if e == nil {
			return nil, false
		}
		_, cfg := e.Config()
		return cfg.Spec.Extensions, true
	}
}

func engineWith(t *testing.T, revision int, entries ...config.Extension) *apply.Engine {
	t.Helper()
	cfg, err := config.Parse([]byte(prunableDoc))
	require.NoError(t, err)
	cfg.Spec.Extensions = entries
	return apply.New(apply.Deps{}, store.Stored{Revision: revision, Config: cfg})
}

func extensionFixture(t *testing.T, log *slog.Logger) (*extension.Store, *extension.Manager) {
	t.Helper()
	dir := t.TempDir()
	images := extension.NewStore(filepath.Join(dir, "extensions"), filepath.Join(dir, "probe"), 1<<20, imageMounter{})
	return images, extension.NewManager(images, imageMounter{}, filepath.Join(dir, "mnt"), "v2", log)
}

func TestPruneExtensions_RemovesWhatNoPinnedEntryOfTheNextBootsConfigurationNames(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, revision := range map[string]int{"revision 0": 0, "after an apply": 3} {
		t.Run(name, func(t *testing.T) {
			images, manager := extensionFixture(t, log)
			pinned := putImage(t, images, manifestOf("tools", "v1"))
			putImage(t, images, manifestOf("naru-orch", "v1"))
			e := engineWith(t, revision, config.Extension{Name: "tools", SHA256: pinned}, config.Extension{Name: "naru-orch"})

			pruneExtensions(specsOf(e), manager, log)

			left, err := manager.List()
			require.NoError(t, err)
			require.Len(t, left, 1, "the unpinned image of another version is removed, as a revision 0 host must too")
			assert.Equal(t, pinned, left[0].SHA256)
		})
	}

	images, manager := extensionFixture(t, log)
	putImage(t, images, manifestOf("naru-orch", "v1"))
	pruneExtensions(specsOf(nil), manager, log)
	left, err := manager.List()
	require.NoError(t, err)
	assert.Len(t, left, 1, "the control: where the boot made no configuration nothing is known to be pinned and nothing is removed")
}

func TestNextBootExtensions_NamesTheEntriesOfTheConfigurationTheNextBootRuns(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	images, manager := extensionFixture(t, log)
	pinned := putImage(t, images, manifestOf("naru-orch", "v1"))

	entries, stored, err := nextBootExtensions(manager, specsOf(engineWith(t, 0, config.Extension{Name: "naru-orch", SHA256: pinned})))
	require.NoError(t, err)
	assert.Equal(t, []upgrade.Entry{{Name: "naru-orch", SHA256: pinned}}, entries, "a revision-0 digest-pinned entry is counted")
	assert.Len(t, stored, 1)

	entries, _, err = nextBootExtensions(manager, specsOf(engineWith(t, 4, config.Extension{Name: "tools"})))
	require.NoError(t, err)
	assert.Equal(t, []upgrade.Entry{{Name: "tools"}}, entries, "the control: after an apply the newest revision's entries are the ones counted")

	entries, stored, err = nextBootExtensions(manager, specsOf(nil))
	require.NoError(t, err)
	assert.Empty(t, entries)
	assert.Empty(t, stored)
}
