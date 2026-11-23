package release_test

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const addonImageScript = "../../../../image/pkgs/addon-image.sh"

// addonImageBuild runs addon-image.sh build over a one-file root file system and returns the archive and
// the reference the script prints.
func addonImageBuild(t *testing.T, name string) (archive, ref string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "rootfs")
	require.NoError(t, os.MkdirAll(root, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("program"), 0o755))
	archive = filepath.Join(dir, name+".tar")
	cmd := exec.Command("sh", addonImageScript, "build", name, name, root, archive)
	cmd.Env = append(os.Environ(), "SOURCE_DATE_EPOCH=1700000000")
	out, err := cmd.Output()
	require.NoError(t, err)
	return archive, strings.TrimSpace(string(out))
}

func addonImageCheck(t *testing.T, name, archive string, manifests ...string) error {
	t.Helper()
	out, err := exec.Command("sh", append([]string{addonImageScript, "check", name, archive}, manifests...)...).CombinedOutput()
	if err != nil {
		t.Logf("%s", out)
	}
	return err
}

func manifestNaming(t *testing.T, refs ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "add-on.yaml")
	var b strings.Builder
	for _, r := range refs {
		b.WriteString("        image: " + r + "\n")
	}
	require.NoError(t, os.WriteFile(p, []byte(b.String()), 0o600))
	return p
}

// renameImage returns the archive with its index's image name replaced.
func renameImage(t *testing.T, archive, from, to string) string {
	t.Helper()
	in, err := os.ReadFile(archive)
	require.NoError(t, err)
	var out bytes.Buffer
	r, w := tar.NewReader(bytes.NewReader(in)), tar.NewWriter(&out)
	renamed := false
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		data, err := io.ReadAll(r)
		require.NoError(t, err)
		if strings.TrimPrefix(h.Name, "./") == "index.json" {
			data = []byte(strings.Replace(string(data), from, to, 1))
			renamed = true
		}
		h.Size = int64(len(data))
		require.NoError(t, w.WriteHeader(h))
		_, err = w.Write(data)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	require.True(t, renamed, "the archive's index.json was found and edited")
	p := filepath.Join(t.TempDir(), "renamed.tar")
	require.NoError(t, os.WriteFile(p, out.Bytes(), 0o600))
	return p
}

// archiveFiles is the members of the tar, by name without a leading "./".
func archiveFiles(t *testing.T, archive string) map[string][]byte {
	t.Helper()
	in, err := os.ReadFile(archive)
	require.NoError(t, err)
	files := map[string][]byte{}
	r := tar.NewReader(bytes.NewReader(in))
	for {
		h, err := r.Next()
		if err == io.EOF {
			return files
		}
		require.NoError(t, err)
		files[strings.TrimPrefix(h.Name, "./")], err = io.ReadAll(r)
		require.NoError(t, err)
	}
}

func TestAddonImageScript_NamesTheImageByTheDigestOfItsManifestSoTheImportCreatesTheNameTheManifestsUse(t *testing.T) {
	archive, ref := addonImageBuild(t, "coredns")
	files := archiveFiles(t, archive)
	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	require.NoError(t, json.Unmarshal(files["index.json"], &index))
	require.Len(t, index.Manifests, 1)
	digest := strings.TrimPrefix(index.Manifests[0].Digest, "sha256:")
	require.Contains(t, files, "blobs/sha256/"+digest, "the index points at a blob the archive holds")
	sum := sha256.Sum256(files["blobs/sha256/"+digest])

	assert.Equal(t, digest, hex.EncodeToString(sum[:]), "the digest is the manifest blob's")
	assert.Equal(t, "naru.local/coredns@sha256:"+digest, index.Manifests[0].Annotations["io.containerd.image.name"], "the import creates this name")
	assert.Equal(t, index.Manifests[0].Annotations["io.containerd.image.name"], ref, "and it is the string the script hands the manifest")
	assert.NoError(t, addonImageCheck(t, "coredns", archive, manifestNaming(t, ref, ref)), "control: a manifest that names the image by that reference is accepted")
}

func TestAddonImageScript_RefusesAManifestWhoseReferenceIsNotTheNameTheArchiveImports(t *testing.T) {
	archive, ref := addonImageBuild(t, "volumes")
	require.NoError(t, addonImageCheck(t, "volumes", archive, manifestNaming(t, ref)), "control: the manifest naming the archive's reference is accepted")

	other := "naru.local/volumes@sha256:" + strings.Repeat("0", 64)

	assert.Error(t, addonImageCheck(t, "volumes", archive, manifestNaming(t, other)), "another digest")
	assert.Error(t, addonImageCheck(t, "volumes", archive, manifestNaming(t, ref, other)), "a second reference that the import does not create")
	assert.Error(t, addonImageCheck(t, "volumes", archive, manifestNaming(t, "naru.local/coredns@"+strings.TrimPrefix(ref, "naru.local/volumes@"))), "another add-on's name")
}

func TestAddonImageScript_RefusesAnArchiveThatNamesItsImageByATagOrByAnotherDigest(t *testing.T) {
	archive, ref := addonImageBuild(t, "ccm")
	manifest := manifestNaming(t, ref)
	require.NoError(t, addonImageCheck(t, "ccm", archive, manifest), "control: the archive as built is accepted")

	tagged := renameImage(t, archive, ref, "naru.local/ccm:release")
	assert.Error(t, addonImageCheck(t, "ccm", tagged, manifestNaming(t, "naru.local/ccm:release")), "a tag: ctr's --digests would add the digest of the index, which no manifest references")

	elsewhere := "naru.local/ccm@sha256:" + strings.Repeat("0", 64)
	assert.Error(t, addonImageCheck(t, "ccm", renameImage(t, archive, ref, elsewhere), manifestNaming(t, elsewhere)), "a digest that is not the manifest blob's")
}
