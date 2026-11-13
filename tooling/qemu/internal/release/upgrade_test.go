package release

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeKernel(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bzImage")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	return p
}

func members(t *testing.T, path string) (names []string, bodies map[string][]byte) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	bodies = map[string][]byte{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names, bodies
		}
		require.NoError(t, err)
		assert.Zero(t, h.ModTime.Unix(), "%s has no time, so the bundle is reproducible", h.Name)
		assert.Zero(t, h.Uid+h.Gid, "%s is owned by root", h.Name)
		names = append(names, h.Name)
		bodies[h.Name], err = io.ReadAll(tr)
		require.NoError(t, err)
	}
}

func TestWriteBundle_HoldsTheKernelAndAManifestThatNamesItsDigest(t *testing.T) {
	kernel := "the kernel with its initramfs"
	out := filepath.Join(t.TempDir(), BundleName("v1.2.3"))

	require.NoError(t, WriteBundle(out, "v1.2.3", writeKernel(t, kernel), 1<<30))

	names, bodies := members(t, out)
	assert.Equal(t, []string{BundleKernel, BundleManifest}, names)
	assert.Equal(t, kernel, string(bodies[BundleKernel]))
	var m BundleManifestJSON
	require.NoError(t, json.Unmarshal(bodies[BundleManifest], &m))
	assert.Equal(t, "v1.2.3", m.Version)
	assert.Equal(t, BundleKernel, m.Kernel.Name)
	assert.EqualValues(t, len(kernel), m.Kernel.Bytes)
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256([]byte(kernel))), m.Kernel.SHA256)
	assert.EqualValues(t, 1<<30, m.ESP.Bytes, "the manifest records the ESP of the image the release was built with")
}

func TestWriteBundle_TheSameInputsGiveTheSameBytes(t *testing.T) {
	k := writeKernel(t, "kernel")
	a, b := filepath.Join(t.TempDir(), "a.tar"), filepath.Join(t.TempDir(), "b.tar")

	require.NoError(t, WriteBundle(a, "v1", k, 1<<30))
	require.NoError(t, WriteBundle(b, "v1", k, 1<<30))

	x, err := os.ReadFile(a)
	require.NoError(t, err)
	y, err := os.ReadFile(b)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(x, y))
}

func TestWriteBundle_RefusesAnEmptyKernel(t *testing.T) {
	err := WriteBundle(filepath.Join(t.TempDir(), "x.tar"), "v1", writeKernel(t, ""), 1<<30)

	assert.ErrorContains(t, err, "is empty")
}

func TestWriteBundle_FailsWhenTheKernelIsMissing(t *testing.T) {
	err := WriteBundle(filepath.Join(t.TempDir(), "x.tar"), "v1", filepath.Join(t.TempDir(), "absent"), 1<<30)

	assert.Error(t, err)
}
