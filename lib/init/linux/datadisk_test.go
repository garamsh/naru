package linux

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/ext4/ext4test"
)

// formatRig is a Host whose mkfs.ext4 is recorded and writes nothing.
type formatRig struct {
	host *Host
	runs [][]string
	logs *bytes.Buffer
	log  *slog.Logger
}

func newFormatRig(t *testing.T, devices map[string]string) *formatRig {
	t.Helper()
	h, _ := testHost(t, devices)
	r := &formatRig{host: h, logs: &bytes.Buffer{}}
	r.log = slog.New(slog.NewTextHandler(r.logs, nil))
	h.run = func(name string, args ...string) ([]byte, error) {
		r.runs = append(r.runs, append([]string{name}, args...))
		return nil, nil
	}
	return r
}

var dataDisk = config.DataDisk{Label: "NARUDATA", MountPoint: "/var/lib/mvmd", FormatBlank: true}

func TestHost_FormatBlankDataDiskFormatsTheOneBlankDiskWithVerityAndTheLabel(t *testing.T) {
	r := newFormatRig(t, map[string]string{"vda": disk(t, 4<<20, nil), "sr0": isoImage(t, "cidata")})

	require.NoError(t, r.host.FormatBlankDataDisk(dataDisk, r.log))

	dev := filepath.Join(r.host.devDir, "vda")
	assert.Equal(t, [][]string{{"/bin/mkfs.ext4", "-q", "-F", "-b", "4096", "-O", "verity", "-L", "NARUDATA", dev}}, r.runs)
	assert.Contains(t, r.logs.String(), "data disk formatted as ext4")
}

func TestHost_FormatBlankDataDiskNeverFormatsADiskThatHoldsAnything(t *testing.T) {
	// A disk whose first MiB has one byte set after a long run of zeros, an ext4 without the
	// label, and a non-ext4 file system are none blank.
	late := make([]byte, 1<<20)
	late[len(late)-1] = 1
	for name, img := range map[string]string{
		"a late byte": disk(t, 4<<20, late),
		"other ext4":  ext4test.Image(t, "OTHER", "verity", nil),
		"an XFS":      disk(t, 4<<20, xfsSuperblock("x", 1)),
	} {
		t.Run(name, func(t *testing.T) {
			r := newFormatRig(t, map[string]string{"vda": img})
			r.host.diskWait = 0

			err := r.host.FormatBlankDataDisk(dataDisk, r.log)

			require.Error(t, err)
			assert.Empty(t, r.runs, "nothing is formatted")
		})
	}
}

func TestHost_FormatBlankDataDiskLeavesTheLabelledDiskAloneOnAReboot(t *testing.T) {
	r := newFormatRig(t, map[string]string{"vda": ext4test.Image(t, "NARUDATA", "verity", nil), "vdb": disk(t, 4<<20, nil)})

	require.NoError(t, r.host.FormatBlankDataDisk(dataDisk, r.log))

	assert.Empty(t, r.runs)
}

func TestHost_FormatBlankDataDiskDoesNotChooseBetweenBlankDisks(t *testing.T) {
	r := newFormatRig(t, map[string]string{"vda": disk(t, 4<<20, nil), "vdb": disk(t, 4<<20, nil)})
	r.host.diskWait = 0

	err := r.host.FormatBlankDataDisk(dataDisk, r.log)

	assert.ErrorContains(t, err, "2 blank disks")
	assert.Empty(t, r.runs)
}

func TestHost_FormatBlankDataDiskSkipsPartitionsAndEmptyDevices(t *testing.T) {
	r := newFormatRig(t, map[string]string{"vda": disk(t, 4<<20, []byte("x")), "vda1": disk(t, 4<<20, nil), "ram0": disk(t, 0, nil)})
	require.NoError(t, os.Remove(filepath.Join(r.host.sysClassBlock, "vda1")))
	require.NoError(t, os.MkdirAll(filepath.Join(r.host.sysClassBlock, "vda1"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(r.host.sysClassBlock, "vda1", "partition"), []byte("1"), 0o644))
	r.host.diskWait = 0

	err := r.host.FormatBlankDataDisk(dataDisk, r.log)

	require.Error(t, err)
	assert.Empty(t, r.runs, "a partition of a used disk and a zero-length device are not blank disks")
}

func TestHost_FormatBlankDataDiskReportsAMkfsFailure(t *testing.T) {
	r := newFormatRig(t, map[string]string{"vda": disk(t, 4<<20, nil)})
	r.host.run = func(string, ...string) ([]byte, error) { return []byte("boom"), assert.AnError }

	err := r.host.FormatBlankDataDisk(dataDisk, r.log)

	assert.ErrorContains(t, err, "mkfs.ext4")
}
