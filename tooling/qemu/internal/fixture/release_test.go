package fixture_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// fakeRelease writes the assets ReadRelease reads: a boot disk, a data disk holding a small
// inputs tree, the guest manifest, the inputs file, the naructl binary and the extension.
func fakeRelease(t *testing.T) string {
	t.Helper()
	dir, work, tree := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "bin"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(tree, "registry"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "bin", "naru-registry"), []byte("registry"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tree, "registry", "index.json"), []byte("{}"), 0o644))
	raw, err := node.BuildReleaseDataDisk(work, tree)
	require.NoError(t, err)
	boot := filepath.Join(work, "boot.img")
	require.NoError(t, os.WriteFile(boot, make([]byte, 1<<20), 0o644))
	for from, to := range map[string]string{raw: "naru-data-v1.qcow2", boot: "naru-v1.qcow2"} {
		out, err := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", from, filepath.Join(dir, to)).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	d := strings.Repeat("a", 64)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "naru-guest-v1.json"), []byte(`{"kernels":{"naru":{"path":"/k","sha256":"`+d+`"}},"default_kernel":"naru",
"guest_initrd":{"path":"/g","sha256":"`+d+`"},"converter_initrd":{"path":"/c","sha256":"`+d+`"}}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fixture.ReleaseInputsName("v1")),
		[]byte(`{"firecrackerSha256":"f1","imageRepository":"127.0.0.1:5000/test/entry","imageDigest":"sha256:`+d+`"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "naructl-v1-linux-amd64"), []byte("#!/bin/sh\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fixture.ReleaseExtensionName("v1", ".sqfs")), []byte("squashfs"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fixture.ReleaseExtensionName("v1", ".json")),
		[]byte(`{"name":"naru-orch","version":"v1","sha256":"`+strings.Repeat("c", 64)+`","services":["naru-orchd","naru-signer"]}`), 0o644))
	return dir
}

func TestReadRelease_ReadsTheInputsTreeFromTheDataDiskAndTheFirecrackerDigestAndImageFromTheInputsFile(t *testing.T) {
	r, err := fixture.ReadRelease(fakeRelease(t), t.TempDir())

	require.NoError(t, err)
	assert.Equal(t, "v1", r.Version)
	assert.FileExists(t, r.BootDisk)
	info, err := os.Stat(filepath.Join(r.Inputs.Dir, "bin", "naru-registry"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "the registry stays executable")
	assert.FileExists(t, filepath.Join(r.Inputs.Dir, "registry", "index.json"))
	assert.Equal(t, "f1", r.Inputs.FirecrackerSHA256)
	assert.Equal(t, fixture.Image{Repository: "127.0.0.1:5000/test/entry", Digest: "sha256:" + strings.Repeat("a", 64)}, r.Inputs.Image)
	assert.Equal(t, "naru", r.Inputs.Guest.DefaultKernel)
	ext := r.Inputs.Extension
	assert.Equal(t, strings.Repeat("c", 64), ext.SHA256)
	assert.Equal(t, []string{"naru-orchd", "naru-signer"}, ext.Services)
	stored, err := os.ReadFile(filepath.Join(ext.StoreDir, ext.SHA256+".sqfs"))
	require.NoError(t, err, "the image is stored under its digest")
	assert.Equal(t, "squashfs", string(stored))
	manifest, err := os.ReadFile(filepath.Join(ext.StoreDir, ext.SHA256+".json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"naru-orch","version":"v1","services":["naru-orchd","naru-signer"]}`, string(manifest), "the manifest the Node's store keeps")
	info, err = os.Stat(r.Naructl)
	require.NoError(t, err, "the release's naructl asset")
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "a downloaded asset is made executable")
}

func TestReadRelease_NamesTheAssetAReleaseLacks(t *testing.T) {
	dir := fakeRelease(t)
	require.NoError(t, os.Remove(filepath.Join(dir, fixture.ReleaseInputsName("v1"))))

	_, err := fixture.ReadRelease(dir, t.TempDir())

	assert.ErrorContains(t, err, "naru-inputs-v1.json")
}

func TestReadRelease_NamesTheNaructlAssetAReleaseLacks(t *testing.T) {
	dir := fakeRelease(t)
	require.NoError(t, os.Remove(filepath.Join(dir, "naructl-v1-linux-amd64")))

	_, err := fixture.ReadRelease(dir, t.TempDir())

	assert.ErrorContains(t, err, "naructl-v1-linux-amd64")
}

func TestReadRelease_NamesTheExtensionAssetAReleaseLacks(t *testing.T) {
	dir := fakeRelease(t)
	require.NoError(t, os.Remove(filepath.Join(dir, fixture.ReleaseExtensionName("v1", ".json"))))

	_, err := fixture.ReadRelease(dir, t.TempDir())

	assert.ErrorContains(t, err, "naru-orch-ext-v1.json")
}

func TestReadExtensionInfo_RefusesADescriptionWithoutADigestOrServices(t *testing.T) {
	p := filepath.Join(t.TempDir(), "naru-orch.json")
	require.NoError(t, os.WriteFile(p, []byte(`{"name":"naru-orch","version":"v1","sha256":"abc","services":[]}`), 0o644))

	_, err := fixture.ReadExtensionInfo(p)

	assert.ErrorContains(t, err, "a version, a SHA-256 and services")
}
