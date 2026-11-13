package release

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The kernels of v0.1.0-dev.7 and v0.1.0-dev.8 (garamsh/naru#106).
const (
	dev7Kernel = 79270912
	dev8Kernel = 211239936
)

// manifestOf is a bundle manifest. esp 0 is a release before ADR 0037, which records none.
func manifestOf(version string, kernel, esp int64) BundleManifestJSON {
	var m BundleManifestJSON
	m.Version = version
	m.Kernel.Bytes = kernel
	m.ESP.Bytes = esp
	return m
}

func writeManifest(t *testing.T, m BundleManifestJSON) string {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(p, b, 0o644))
	return p
}

func TestESPBytes_ReadsTheSizeOfTheFirstGPTPartition(t *testing.T) {
	disk := make([]byte, 4096)
	binary.LittleEndian.PutUint64(disk[firstEntryOffset+32:], 2048)
	binary.LittleEndian.PutUint64(disk[firstEntryOffset+40:], 2048+2*1024*1024-1)
	p := filepath.Join(t.TempDir(), "naru.img")
	require.NoError(t, os.WriteFile(p, disk, 0o644))

	n, err := ESPBytes(p)

	require.NoError(t, err)
	assert.EqualValues(t, 1<<30, n)
}

func TestESPBytes_FailsOnADiskWithoutAPartitionEntry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "naru.img")
	require.NoError(t, os.WriteFile(p, make([]byte, 4096), 0o644))

	_, err := ESPBytes(p)

	assert.ErrorContains(t, err, "no partition")
}

func TestESPOf_UsesTheManifestsFieldAndFallsBackToTheFormulaOfTheTimeWhereThereIsNone(t *testing.T) {
	tests := []struct {
		name string
		m    BundleManifestJSON
		want int64
	}{
		{"a field is taken as it is", manifestOf("v1", dev8Kernel, 1<<30), 1 << 30},
		{"no field: two kernels and 8 MiB, rounded up to a MiB (dev.7's kernel gives 160 MiB)", manifestOf("v0.1.0-dev.7", dev7Kernel, 0), 160 * mib},
		{"no field: a small kernel gets the 64 MiB minimum", manifestOf("v0.1.0", 10*mib, 0), 64 * mib},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.EqualValues(t, tc.want, tc.m.ESPOf())
		})
	}
}

func TestCheckSuccessor_RefusesAKernelThatDoesNotFitTheTrialSlotOfThePreviousImage(t *testing.T) {
	tests := []struct {
		name    string
		prev    BundleManifestJSON
		kernel  int64
		refused bool
	}{
		{"dev.7 has no ESP field, and dev.8's kernel does not fit its formula's ESP", manifestOf("v0.1.0-dev.7", dev7Kernel, 0), dev8Kernel, true},
		{"control: dev.7's own size still fits the formula's ESP", manifestOf("v0.1.0-dev.7", dev7Kernel, 0), dev7Kernel, false},
		{"a recorded ESP of 1 GiB takes a kernel twice dev.8's", manifestOf("v0.1.0-dev.9", dev8Kernel, 1<<30), 2 * dev8Kernel, false},
		{"a recorded ESP of 1 GiB refuses a kernel of 1 GiB", manifestOf("v0.1.0-dev.9", dev8Kernel, 1<<30), 1 << 30, true},
		{"the recorded field wins over the formula: dev.8's kernel fits a recorded 1 GiB and not the formula's ESP", manifestOf("v0.1.0-dev.9", dev7Kernel, 1<<30), dev8Kernel, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSuccessor(tc.prev, tc.kernel)

			if tc.refused {
				assert.ErrorContains(t, err, "does not fit the trial slot")
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCheckPrevious_RefusesUnlessTheReleaseIsMarkedAsRequiringRecreation(t *testing.T) {
	kernel := filepath.Join(t.TempDir(), "bzImage")
	require.NoError(t, os.WriteFile(kernel, make([]byte, 100*mib), 0o644))
	small := manifestOf("v0.1.0-dev.7", 10*mib, 0)
	require.NoError(t, CheckSuccessor(small, 10*mib), "the previous kernel's own size fits")
	require.Error(t, CheckSuccessor(small, 100*mib), "the new kernel does not fit")

	assert.ErrorContains(t, checkPrevious(Options{PrevManifest: writeManifest(t, small)}, kernel), "RECREATE=1")
	assert.NoError(t, checkPrevious(Options{PrevManifest: writeManifest(t, small), RequiresRecreation: true}, kernel))
	assert.NoError(t, checkPrevious(Options{}, kernel), "with no previous manifest nothing is checked")
	assert.NoError(t, checkPrevious(Options{PrevManifest: writeManifest(t, manifestOf("v1", 10*mib, 1<<30))}, kernel), "control: a previous image with room accepts it")
}

func TestReadManifest_FailsOnAManifestWithoutAKernelSize(t *testing.T) {
	_, err := ReadManifest(writeManifest(t, manifestOf("v1", 0, 0)))

	assert.ErrorContains(t, err, "no kernel size")
}
