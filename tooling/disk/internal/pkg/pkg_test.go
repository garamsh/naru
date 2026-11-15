package pkg_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/disk/internal/pkg"
)

type entry struct {
	name, body, link string
	mode             int64
	typ              byte
}

func file(name, body string, mode int64) entry {
	return entry{name: name, body: body, mode: mode, typ: tar.TypeReg}
}
func dir(name string) entry { return entry{name: name, mode: 0o755, typ: tar.TypeDir} }
func symlink(name, to string) entry {
	return entry{name: name, link: to, mode: 0o777, typ: tar.TypeSymlink}
}

func layerTarball(t *testing.T, entries []entry, gz bool) []byte {
	t.Helper()
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, e := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: e.name, Mode: e.mode, Typeflag: e.typ, Size: int64(len(e.body)), Linkname: e.link}))
		_, err := tw.Write([]byte(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	if !gz {
		return raw.Bytes()
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	_, err := zw.Write(raw.Bytes())
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return out.Bytes()
}

func digest(b []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(b)) }

// image writes an OCI layout tar whose image has the layers given, in order, and returns its path
// and manifest digest. The blobs are written in reverse so that the layout does not rely on order.
func image(t *testing.T, layers ...[]byte) (string, string) {
	t.Helper()
	type desc struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int    `json:"size"`
	}
	var ds []desc
	blobs := map[string][]byte{}
	for _, l := range layers {
		media := "application/vnd.oci.image.layer.v1.tar"
		if len(l) > 2 && l[0] == 0x1f && l[1] == 0x8b {
			media += "+gzip"
		}
		ds = append(ds, desc{media, digest(l), len(l)})
		blobs[digest(l)] = l
	}
	manifest, err := json.Marshal(map[string]any{"schemaVersion": 2, "layers": ds})
	require.NoError(t, err)
	blobs[digest(manifest)] = manifest
	index, err := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": []desc{{"application/vnd.oci.image.manifest.v1+json", digest(manifest), len(manifest)}}})
	require.NoError(t, err)

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	put := func(name string, b []byte) {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b)), Typeflag: tar.TypeReg}))
		_, err := tw.Write(b)
		require.NoError(t, err)
	}
	for d, b := range blobs {
		put("blobs/sha256/"+d[len("sha256:"):], b)
	}
	put("index.json", index)
	require.NoError(t, tw.Close())
	path := filepath.Join(t.TempDir(), "p.tar")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	return path, digest(manifest)
}

func TestDigest_IsTheManifestDigest(t *testing.T) {
	path, want := image(t, layerTarball(t, []entry{file("usr/bin/a", "a", 0o755)}, false))
	got, err := pkg.Digest(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestUnpack_AppliesLayersInOrderWithWhiteouts(t *testing.T) {
	lower := layerTarball(t, []entry{
		dir("usr"), dir("usr/bin"), file("usr/bin/keep", "lower", 0o755), file("usr/bin/gone", "x", 0o755),
		dir("etc"), file("etc/old", "x", 0o644), symlink("usr/bin/link", "keep"),
	}, false)
	upper := layerTarball(t, []entry{
		file("usr/bin/keep", "upper", 0o755), file("usr/bin/.wh.gone", "", 0o644),
		file("etc/.wh..wh..opq", "", 0o644), file("etc/new", "n", 0o644),
	}, true)
	path, _ := image(t, lower, upper)
	out := filepath.Join(t.TempDir(), "p")
	require.NoError(t, pkg.Unpack(path, out))

	b, err := os.ReadFile(filepath.Join(out, "usr/bin/keep"))
	require.NoError(t, err)
	assert.Equal(t, "upper", string(b))
	assert.NoFileExists(t, filepath.Join(out, "usr/bin/gone"))
	assert.NoFileExists(t, filepath.Join(out, "etc/old"))
	assert.FileExists(t, filepath.Join(out, "etc/new"))
	to, err := os.Readlink(filepath.Join(out, "usr/bin/link"))
	require.NoError(t, err)
	assert.Equal(t, "keep", to)
	fi, err := os.Stat(filepath.Join(out, "usr/bin/keep"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), fi.Mode().Perm())
}

func TestUnpack_KeepsAnEntryInsideTheDirectory(t *testing.T) {
	path, _ := image(t, layerTarball(t, []entry{file("../../escape", "x", 0o644)}, false))
	parent := t.TempDir()
	require.NoError(t, pkg.Unpack(path, filepath.Join(parent, "p", "out")))
	assert.FileExists(t, filepath.Join(parent, "p", "out", "escape"))
	assert.NoFileExists(t, filepath.Join(parent, "escape"))
}

func TestUnpack_RefusesATarThatIsNotAnImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.tar")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "a", Size: 1, Mode: 0o644, Typeflag: tar.TypeReg}))
	_, _ = tw.Write([]byte("a"))
	require.NoError(t, tw.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	assert.ErrorContains(t, pkg.Unpack(path, filepath.Join(t.TempDir(), "o")), "no index.json")
}

func TestDigest_RefusesAnIndexThatIsNotOneImageManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.tar")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	index := []byte(`{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.index.v1+json","digest":"sha256:00","size":1}]}`)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "index.json", Size: int64(len(index)), Mode: 0o644, Typeflag: tar.TypeReg}))
	_, _ = tw.Write(index)
	require.NoError(t, tw.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
	_, err := pkg.Digest(path)
	assert.ErrorContains(t, err, "exactly one image manifest")
}
