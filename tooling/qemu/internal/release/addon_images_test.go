package release_test

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

const addonDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// ociArchive is the tar of an OCI layout whose index names one manifest.
func ociArchive(t *testing.T, digest string) string {
	t.Helper()
	index := `{"schemaVersion":2,"manifests":[{"digest":"` + digest + `"}]}`
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	require.NoError(t, w.WriteHeader(&tar.Header{Name: "index.json", Mode: 0o644, Size: int64(len(index))}))
	_, err := w.Write([]byte(index))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	p := filepath.Join(t.TempDir(), "coredns.tar")
	require.NoError(t, os.WriteFile(p, b.Bytes(), 0o600))
	return p
}

func TestReadAddonImages_NamesEachAddonByTheManifestDigestOfItsArchiveAndSkipsOtherFiles(t *testing.T) {
	other := filepath.Join(t.TempDir(), "other")
	require.NoError(t, os.WriteFile(other, []byte("not an add-on"), 0o600))
	archive := newc(t, [][2]string{{"usr/lib/naru/addons/images/coredns.tar", ociArchive(t, addonDigest)}, {"usr/lib/naru/addons/manifests/dns.yaml", other}})

	images, err := release.ReadAddonImages([]string{archive})

	require.NoError(t, err)
	assert.Equal(t, []release.AddonImage{{Name: "coredns", Digest: addonDigest}}, images)
}

func TestReadAddonImages_RefusesTwoDigestsForOneAddonAndAcceptsTheSameDigestTwice(t *testing.T) {
	same := newc(t, [][2]string{{"usr/lib/naru/addons/images/coredns.tar", ociArchive(t, addonDigest)}})
	again := newc(t, [][2]string{{"usr/lib/naru/addons/images/coredns.tar", ociArchive(t, addonDigest)}})
	differ := newc(t, [][2]string{{"usr/lib/naru/addons/images/coredns.tar", ociArchive(t, "sha256:"+string(bytes.Repeat([]byte("a"), 64)))}})

	images, err := release.ReadAddonImages([]string{same, again})
	require.NoError(t, err)
	assert.Len(t, images, 1, "the Node and the host initramfs carry the same image")

	_, err = release.ReadAddonImages([]string{same, differ})
	assert.Error(t, err)
}

func TestSBOM_StatesTheDigestOfEachAddonImage(t *testing.T) {
	doc, err := release.SBOM("v9", epoch, nil, nil, []release.AddonImage{{Name: "coredns", Digest: addonDigest}})

	require.NoError(t, err)
	assert.Contains(t, string(doc), `"name": "addon-image-coredns"`)
	assert.Contains(t, string(doc), `"checksumValue": "`+addonDigest[len("sha256:"):]+`"`, "the package carries the digest as its checksum")
}
