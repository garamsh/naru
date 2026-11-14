package extension_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
)

const version = "v1.2.3"

// fakeMounter stands for the kernel: an "image" is the text of its manifest.json, and mounting one
// writes that file into the directory. An image that does not start with "{" cannot be mounted.
type fakeMounter struct {
	mounted  map[string]string // dir -> image content
	mountErr error
}

func (f *fakeMounter) Mount(image *os.File, dir string) error {
	if f.mountErr != nil {
		return f.mountErr
	}
	data, err := io.ReadAll(io.NewSectionReader(image, 0, 1<<20))
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(data, []byte("{")) {
		return errors.New("wrong fs type")
	}
	if f.mounted == nil {
		f.mounted = map[string]string{}
	}
	f.mounted[dir] = string(data)
	return os.WriteFile(filepath.Join(dir, extension.ManifestPath), data, 0o644)
}

func (f *fakeMounter) Unmount(dir string) error {
	delete(f.mounted, dir)
	return os.Remove(filepath.Join(dir, extension.ManifestPath))
}

func manifest(name, version string) string {
	return `{"name":"` + name + `","version":"` + version + `","services":["naru-orchd","naru-signer"]}`
}

func digestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func newStore(t *testing.T, m *fakeMounter) (*extension.Store, string) {
	t.Helper()
	dir := t.TempDir()
	return extension.NewStore(filepath.Join(dir, "extensions"), filepath.Join(dir, "probe"), 1<<20, m), dir
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestStore_PutKeepsTheImageUnderItsDigestWithItsManifest(t *testing.T) {
	m := &fakeMounter{}
	s, root := newStore(t, m)
	content := manifest("naru-orch", version)

	img, err := s.Put(strings.NewReader(content), digestOf(content))

	require.NoError(t, err)
	assert.Equal(t, extension.Image{SHA256: digestOf(content), Size: int64(len(content)), Manifest: extension.Manifest{
		Name: "naru-orch", Version: version, Services: []string{"naru-orchd", "naru-signer"},
	}}, img)
	assert.ElementsMatch(t, []string{digestOf(content) + ".sqfs", digestOf(content) + ".json"}, names(t, filepath.Join(root, "extensions")), "nothing is left but the image and its manifest")
	assert.Empty(t, m.mounted, "the probe mount is gone")
	assert.Empty(t, names(t, filepath.Join(root, "probe")))
	list, err := s.List()
	require.NoError(t, err)
	assert.Equal(t, []extension.Image{img}, list)
}

func TestStore_PutRefusesBytesThatAreNotTheDigest(t *testing.T) {
	// The control is the test above: with the digest of the bytes the image is kept.
	s, root := newStore(t, &fakeMounter{})
	content := manifest("naru-orch", version)

	_, err := s.Put(strings.NewReader(content), digestOf("other"))

	require.ErrorIs(t, err, extension.ErrDigest)
	assert.Empty(t, names(t, filepath.Join(root, "extensions")), "the temporary file is removed")
}

func TestStore_PutRefusesADigestThatIsNotLowerCaseHex(t *testing.T) {
	s, _ := newStore(t, &fakeMounter{})
	content := manifest("naru-orch", version)

	_, err := s.Put(strings.NewReader(content), strings.ToUpper(digestOf(content)))

	require.ErrorIs(t, err, extension.ErrDigest)
}

func TestStore_PutRefusesWhatIsNotAnImage(t *testing.T) {
	s, root := newStore(t, &fakeMounter{})
	content := "this is not squashfs"

	_, err := s.Put(strings.NewReader(content), digestOf(content))

	require.ErrorIs(t, err, extension.ErrImage)
	assert.Empty(t, names(t, filepath.Join(root, "extensions")))
}

func TestStore_PutRefusesAnImageWithoutAUsableManifest(t *testing.T) {
	for name, content := range map[string]string{
		"no version":      `{"name":"naru-orch","services":["a"]}`,
		"no services":     `{"name":"naru-orch","version":"v1"}`,
		"an unknown key":  `{"name":"naru-orch","version":"v1","services":["a"],"x":1}`,
		"not json at all": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			s, root := newStore(t, &fakeMounter{})

			_, err := s.Put(strings.NewReader(content), digestOf(content))

			require.ErrorIs(t, err, extension.ErrImage)
			assert.Empty(t, names(t, filepath.Join(root, "extensions")))
		})
	}
}

func TestStore_PutRefusesAnImageLargerThanTheCap(t *testing.T) {
	m := &fakeMounter{}
	dir := t.TempDir()
	s := extension.NewStore(filepath.Join(dir, "e"), filepath.Join(dir, "p"), 10, m)
	content := manifest("naru-orch", version)

	_, err := s.Put(strings.NewReader(content), digestOf(content))

	require.ErrorIs(t, err, extension.ErrTooLarge)
	assert.Empty(t, names(t, filepath.Join(dir, "e")))
}

func TestStore_PutOfAStoredImageChangesNothing(t *testing.T) {
	s, _ := newStore(t, &fakeMounter{})
	content := manifest("naru-orch", version)
	first, err := s.Put(strings.NewReader(content), digestOf(content))
	require.NoError(t, err)

	second, err := s.Put(strings.NewReader(content), digestOf(content))

	require.NoError(t, err)
	assert.Equal(t, first, second)
}

func TestStore_DeleteRemovesTheImageAndItsManifest(t *testing.T) {
	s, root := newStore(t, &fakeMounter{})
	content := manifest("naru-orch", version)
	_, err := s.Put(strings.NewReader(content), digestOf(content))
	require.NoError(t, err)

	require.NoError(t, s.Delete(digestOf(content)))

	assert.Empty(t, names(t, filepath.Join(root, "extensions")))
	require.ErrorIs(t, s.Delete(digestOf(content)), extension.ErrNotFound)
}

func TestStore_OpenRefusesAFileThatChangedOnTheDisk(t *testing.T) {
	s, root := newStore(t, &fakeMounter{})
	content := manifest("naru-orch", version)
	_, err := s.Put(strings.NewReader(content), digestOf(content))
	require.NoError(t, err)
	f, _, err := s.Open(digestOf(content))
	require.NoError(t, err, "the control: an untouched image opens")
	f.Close()

	require.NoError(t, os.WriteFile(filepath.Join(root, "extensions", digestOf(content)+".sqfs"), []byte(manifest("naru-orch", "other")), 0o600))
	_, _, err = s.Open(digestOf(content))

	require.ErrorIs(t, err, extension.ErrDigest)
}

func TestStore_SweepRemovesTheLeftoversOfAnUnfinishedUpload(t *testing.T) {
	s, root := newStore(t, &fakeMounter{})
	content := manifest("naru-orch", version)
	_, err := s.Put(strings.NewReader(content), digestOf(content))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "extensions", ".put-123"), []byte("half"), 0o600))

	require.NoError(t, s.Sweep())

	assert.ElementsMatch(t, []string{digestOf(content) + ".sqfs", digestOf(content) + ".json"}, names(t, filepath.Join(root, "extensions")))
}

// node is a Manager over a store and a mount root in a temporary directory.
type node struct {
	m    *fakeMounter
	s    *extension.Store
	mgr  *extension.Manager
	root string
}

func newNode(t *testing.T) *node {
	t.Helper()
	m := &fakeMounter{}
	s, dir := newStore(t, m)
	root := filepath.Join(dir, "mnt")
	return &node{m: m, s: s, mgr: extension.NewManager(s, m, root, version, slog.New(slog.NewTextHandler(io.Discard, nil))), root: root}
}

func (n *node) put(t *testing.T, content string) string {
	t.Helper()
	_, err := n.s.Put(strings.NewReader(content), digestOf(content))
	require.NoError(t, err)
	return digestOf(content)
}

func TestManager_MountPicksTheImageBuiltForInitsVersionAndMountsItReadOnlyUnderItsName(t *testing.T) {
	n := newNode(t)
	n.put(t, manifest("naru-orch", "v0.9.0"))
	want := n.put(t, manifest("naru-orch", version))

	err := n.mgr.Mount([]config.Extension{{Name: "naru-orch", SHA256: want}})

	require.NoError(t, err)
	assert.Equal(t, manifest("naru-orch", version), n.m.mounted[filepath.Join(n.root, "naru-orch")])
	list, err := n.mgr.List()
	require.NoError(t, err)
	var mounted []string
	for _, e := range list {
		if e.Mounted {
			mounted = append(mounted, e.SHA256)
			assert.Equal(t, filepath.Join(n.root, "naru-orch"), e.MountPoint)
		}
	}
	assert.Equal(t, []string{want}, mounted, "only the image of this version is mounted; the other is stored")
	assert.Len(t, list, 2)
}

func TestManager_MountRefuses(t *testing.T) {
	good := manifest("naru-orch", version)
	tests := []struct {
		name  string
		setup func(t *testing.T, n *node) string // returns the configured digest
		want  string
	}{
		{"an extension nothing was stored for", func(t *testing.T, n *node) string { return digestOf("x") }, "no image is stored"},
		{"an image built for another version", func(t *testing.T, n *node) string {
			return n.put(t, manifest("naru-orch", "v0.9.0"))
		}, "not built for naru " + version},
		{"an image under another name", func(t *testing.T, n *node) string {
			return n.put(t, manifest("other", version))
		}, `is the extension "other"`},
		{"a configured digest that is not the one stored for this version", func(t *testing.T, n *node) string {
			n.put(t, good)
			return digestOf("another")
		}, "not the configured"},
		{"an image whose file changed on the disk", func(t *testing.T, n *node) string {
			d := n.put(t, good)
			require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(n.root), "extensions", d+".sqfs"), []byte(manifest("naru-orch", version)+" "), 0o600))
			return d
		}, extension.ErrDigest.Error()},
		{"an image whose manifest differs from the stored one", func(t *testing.T, n *node) string {
			d := n.put(t, good)
			require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(n.root), "extensions", d+".json"), []byte(manifest("naru-orch", version)[:len(good)-len(`,"services":["naru-orchd","naru-signer"]}`)]+`,"services":["naru-orchd"]}`), 0o600))
			return d
		}, "differs from the one stored"},
	}
	{
		n := newNode(t)
		d := n.put(t, good)
		require.NoError(t, n.mgr.Mount([]config.Extension{{Name: "naru-orch", SHA256: d}}), "the control: the good image mounts")
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNode(t)
			digest := tt.setup(t, n)

			err := n.mgr.Mount([]config.Extension{{Name: "naru-orch", SHA256: digest}})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Contains(t, err.Error(), `extension "naru-orch"`)
			assert.Empty(t, n.m.mounted, "nothing stays mounted")
		})
	}
}

func TestManager_DeleteRefusesAMountedImageAndRemovesAnUnmountedOne(t *testing.T) {
	n := newNode(t)
	mounted := n.put(t, manifest("naru-orch", version))
	next := n.put(t, manifest("naru-orch", "v1.3.0"))
	require.NoError(t, n.mgr.Mount([]config.Extension{{Name: "naru-orch", SHA256: mounted}}))

	require.ErrorIs(t, n.mgr.Delete(mounted), extension.ErrMounted)
	require.NoError(t, n.mgr.Delete(next))

	list, err := n.mgr.List()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, mounted, list[0].SHA256)
}

func TestManager_PutReportsTheEntryAsNotMounted(t *testing.T) {
	n := newNode(t)
	content := manifest("naru-orch", version)

	e, err := n.mgr.Put(strings.NewReader(content), digestOf(content))

	require.NoError(t, err)
	assert.False(t, e.Mounted)
	assert.Equal(t, "naru-orch", e.Name)
}
