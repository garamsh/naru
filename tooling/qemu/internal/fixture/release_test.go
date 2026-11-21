package fixture_test

import (
	"crypto/sha256"
	"fmt"
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
func fakeRelease(t *testing.T) string { return fakeReleaseOf(t, "v1") }

// fakeReleaseOf is fakeRelease for the version, with the volume extension a release made after
// ADR 0083 carries when the version is one that does.
func fakeReleaseOf(t *testing.T, version string) string {
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
	for from, to := range map[string]string{raw: "naru-data-" + version + ".qcow2", boot: fixture.BootDiskName(version)} {
		out, err := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", from, filepath.Join(dir, to)).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	d := strings.Repeat("a", 64)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "naru-guest-"+version+".json"), []byte(`{"kernels":{"naru":{"path":"/k","sha256":"`+d+`"}},"default_kernel":"naru",
"guest_initrd":{"path":"/g","sha256":"`+d+`"},"converter_initrd":{"path":"/c","sha256":"`+d+`"}}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fixture.ReleaseInputsName(version)),
		[]byte(`{"firecrackerSha256":"f1","imageRepository":"127.0.0.1:5000/test/entry","imageDigest":"sha256:`+d+`"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "naructl-"+version+"-linux-amd64"), []byte("#!/bin/sh\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fixture.ReleaseExtensionName(version, ".sqfs")), []byte("squashfs"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fixture.ReleaseExtensionName(version, ".json")),
		[]byte(`{"name":"naru-orch","version":"v1","sha256":"`+strings.Repeat("c", 64)+`","services":["naru-orchd","naru-signer"]}`), 0o644))
	if !fixture.PredatesVolumes(version) {
		addVolumes(t, dir, version, "volumes")
	}
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

func TestBootDiskName_IsNaruHostAndTheVersion(t *testing.T) {
	assert.Equal(t, "naru-host-v1.qcow2", fixture.BootDiskName("v1"))
}

func TestReadRelease_ReadsTheBootDiskOfAReleaseMadeBeforeTheRename(t *testing.T) {
	dir := fakeRelease(t)
	require.NoError(t, os.Rename(filepath.Join(dir, "naru-host-v1.qcow2"), filepath.Join(dir, "naru-v1.qcow2")))

	r, err := fixture.ReadRelease(dir, t.TempDir())

	require.NoError(t, err)
	assert.FileExists(t, r.BootDisk)
}

func TestReadRelease_PrefersTheNaruHostBootDiskWhenBothNamesExist(t *testing.T) {
	dir := fakeRelease(t)
	big := filepath.Join(t.TempDir(), "big.img")
	require.NoError(t, os.WriteFile(big, make([]byte, 2<<20), 0o644))
	out, err := exec.Command("qemu-img", "convert", "-f", "raw", "-O", "qcow2", big, filepath.Join(dir, "naru-v1.qcow2")).CombinedOutput()
	require.NoError(t, err, string(out))

	r, err := fixture.ReadRelease(dir, t.TempDir())

	require.NoError(t, err)
	info, err := os.Stat(r.BootDisk)
	require.NoError(t, err)
	assert.Equal(t, int64(1<<20), info.Size(), "the 1 MiB naru-host disk, not the 2 MiB legacy one")
}

func TestReadRelease_NamesBothBootDiskNamesAReleaseLacks(t *testing.T) {
	dir := fakeRelease(t)
	_, err := fixture.ReadRelease(dir, t.TempDir())
	require.NoError(t, err, "control: the release is readable with its boot disk")
	require.NoError(t, os.Remove(filepath.Join(dir, "naru-host-v1.qcow2")))

	_, err = fixture.ReadRelease(dir, t.TempDir())

	assert.ErrorContains(t, err, "naru-host-v1.qcow2")
	assert.ErrorContains(t, err, "naru-v1.qcow2")
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

// addVolumes writes the volume extension's assets of the release version, with the description of image.
func addVolumes(t *testing.T, dir, version, image string) {
	t.Helper()
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("volumes")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fixture.ReleaseVolumesExtensionName(version, ".sqfs")), []byte(image), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, fixture.ReleaseVolumesExtensionName(version, ".json")),
		[]byte(`{"name":"naru-volumes","version":"`+version+`","sha256":"`+sum+`","services":[]}`), 0o644))
}

func TestReadRelease_StoresTheVolumeExtensionBesideNaruOrchsSoEveryNodeFindsIt(t *testing.T) {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("volumes")))

	r, err := fixture.ReadRelease(fakeRelease(t), t.TempDir())

	require.NoError(t, err)
	store := r.Inputs.Extension.StoreDir
	image, err := os.ReadFile(filepath.Join(store, sum+".sqfs"))
	require.NoError(t, err, "stored under its digest in the store naru-orch is in")
	assert.Equal(t, "volumes", string(image))
	manifest, err := os.ReadFile(filepath.Join(store, sum+".json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"naru-volumes","version":"v1","services":[]}`, string(manifest), "an empty list of services, not null")
	assert.FileExists(t, filepath.Join(store, r.Inputs.Extension.SHA256+".sqfs"), "naru-orch is still there")
}

func TestReadRelease_AReleaseBeforeTheVolumeExtensionStoresOnlyNaruOrch(t *testing.T) {
	r, err := fixture.ReadRelease(fakeReleaseOf(t, "v0.1.0-dev.13"), t.TempDir())

	require.NoError(t, err)
	entries, err := os.ReadDir(r.Inputs.Extension.StoreDir)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "naru-orch's image and manifest")
}

func TestReadRelease_RefusesACurrentReleaseThatLacksTheVolumeExtension(t *testing.T) {
	for _, version := range []string{"v1", "v0.1.0-dev.14", "v0.1.0-dev.13-12-gb9a6a10"} {
		dir := fakeReleaseOf(t, version)
		_, err := fixture.ReadRelease(dir, t.TempDir())
		require.NoError(t, err, "the control: %s with its volume extension is read", version)
		require.NoError(t, os.Remove(filepath.Join(dir, fixture.ReleaseVolumesExtensionName(version, ".json"))))

		_, err = fixture.ReadRelease(dir, t.TempDir())

		assert.ErrorContains(t, err, fixture.ReleaseVolumesExtensionName(version, ".json"), version)
	}
}

func TestReadRelease_RefusesAVolumeExtensionWhoseImageIsNotTheOneItsDescriptionStates(t *testing.T) {
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("volumes")))
	for name, tc := range map[string]struct{ image, errPart string }{
		"another image": {"changed", "states SHA-256 " + sum},
		"the image":     {"volumes", ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := fakeRelease(t)
			addVolumes(t, dir, "v1", tc.image)

			_, err := fixture.ReadRelease(dir, t.TempDir())

			if tc.errPart == "" {
				assert.NoError(t, err, "the control: the described image is accepted")
				return
			}
			assert.ErrorContains(t, err, tc.errPart)
		})
	}
}

func TestReadVolumesExtensionInfo_RefusesAnotherNameAServiceOrAMalformedDigest(t *testing.T) {
	sum := strings.Repeat("e", 64)
	for name, info := range map[string]string{
		"another name":     `{"name":"naru-orch","version":"v9","sha256":"` + sum + `","services":[]}`,
		"a service":        `{"name":"naru-volumes","version":"v9","sha256":"` + sum + `","services":["x"]}`,
		"no version":       `{"name":"naru-volumes","sha256":"` + sum + `","services":[]}`,
		"a malformed hash": `{"name":"naru-volumes","version":"v9","sha256":"zz","services":[]}`,
		"the description":  `{"name":"naru-volumes","version":"v9","sha256":"` + sum + `","services":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "naru-volumes.json")
			require.NoError(t, os.WriteFile(p, []byte(info), 0o644))

			i, err := fixture.ReadVolumesExtensionInfo(p)

			if name == "the description" {
				require.NoError(t, err, "the control: a well-formed description is accepted")
				assert.NotNil(t, i.Services)
				return
			}
			assert.Error(t, err)
		})
	}
}
