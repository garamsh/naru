package linux

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/ext4/ext4test"
	"github.com/garamsh/naru/lib/wire"
)

var firstBlankDisk = config.DataDisk{Label: "NARUDATA", MountPoint: "/var/lib/naru", FormatBlank: true, FirstBlank: true}

// withBootPartition makes the ESP of the image a partition of the disk name in sysfs, as the
// kernel shows it: a directory under the disk's, linked from /sys/class/block.
func withBootPartition(t *testing.T, h *Host, name, partuuid string) {
	t.Helper()
	dir := filepath.Join(filepath.Dir(filepath.Dir(h.sysClassBlock)), "devices", name, name+"1")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "partition"), []byte("1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "uevent"), []byte("DEVTYPE=partition\nPARTUUID="+partuuid+"\n"), 0o644))
	require.NoError(t, os.Symlink(dir, filepath.Join(h.sysClassBlock, name+"1")))
}

func formatted(r *formatRig) string { return r.runs[0][len(r.runs[0])-1] }

func TestHost_FormatBlankDataDiskTakesTheFirstBlankDiskThatIsNotTheBootDiskForAJoinForm(t *testing.T) {
	devices := map[string]string{"vda": disk(t, 4<<20, nil), "vdb": disk(t, 4<<20, nil), "vdc": disk(t, 4<<20, nil)}

	r := newFormatRig(t, devices)
	withBootPartition(t, r.host, "vda", wire.ESPGUID)
	require.NoError(t, r.host.FormatBlankDataDisk(firstBlankDisk, r.log))
	assert.Equal(t, filepath.Join(r.host.devDir, "vdb"), formatted(r), "vda holds the ESP, so vdb is the first blank disk")

	// The control: with another PARTUUID on vda's partition, vda is the first blank disk.
	r = newFormatRig(t, devices)
	withBootPartition(t, r.host, "vda", "11111111-2222-3333-4444-555555555555")
	require.NoError(t, r.host.FormatBlankDataDisk(firstBlankDisk, r.log))
	assert.Equal(t, filepath.Join(r.host.devDir, "vda"), formatted(r))

	// The control: the other form still refuses to choose between blank disks.
	r = newFormatRig(t, devices)
	r.host.diskWait = 0
	assert.ErrorContains(t, r.host.FormatBlankDataDisk(dataDisk, r.log), "3 blank disks")
}

func TestHost_FormatBlankDataDiskSkipsTheDiskHoldingTheESPWhateverItsName(t *testing.T) {
	devices := map[string]string{"sda": disk(t, 4<<20, nil), "sdb": disk(t, 4<<20, nil)}

	r := newFormatRig(t, devices)
	withBootPartition(t, r.host, "sdb", wire.ESPGUID)
	require.NoError(t, r.host.FormatBlankDataDisk(firstBlankDisk, r.log))
	assert.Equal(t, filepath.Join(r.host.devDir, "sda"), formatted(r), "the ESP is on sdb, so sda is the first blank disk")

	// The control: with the ESP on sda, the first blank disk is sdb, so the skipped disk follows the ESP.
	r = newFormatRig(t, devices)
	withBootPartition(t, r.host, "sda", wire.ESPGUID)
	require.NoError(t, r.host.FormatBlankDataDisk(firstBlankDisk, r.log))
	assert.Equal(t, filepath.Join(r.host.devDir, "sdb"), formatted(r))
}

func TestHost_FormatBlankDataDiskPrefersTheLabelledDiskToABlankOneForAJoinForm(t *testing.T) {
	r := newFormatRig(t, map[string]string{"vda": disk(t, 4<<20, nil), "vdb": ext4test.Image(t, "NARUDATA", "verity", nil)})

	require.NoError(t, r.host.FormatBlankDataDisk(firstBlankDisk, r.log))

	assert.Empty(t, r.runs)
}

func TestHost_FormatBlankDataDiskRefusesAJoinFormWhenOnlyTheBootDiskIsBlank(t *testing.T) {
	r := newFormatRig(t, map[string]string{"vda": disk(t, 4<<20, nil)})
	withBootPartition(t, r.host, "vda", wire.ESPGUID)
	r.host.diskWait = 0

	err := r.host.FormatBlankDataDisk(firstBlankDisk, r.log)

	assert.ErrorIs(t, err, errNoDisk)
	assert.Empty(t, r.runs)
}

func TestHost_FormatBlankDataDiskDoesNotCallADiskBlankWhoseLastMiBHoldsABackupTable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "gpt")
	f, err := os.Create(p)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(4<<20))
	_, err = f.WriteAt([]byte("EFI PART"), 4<<20-512)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	r := newFormatRig(t, map[string]string{"vda": p, "vdb": disk(t, 4<<20, nil)})

	require.NoError(t, r.host.FormatBlankDataDisk(firstBlankDisk, r.log))

	assert.Equal(t, filepath.Join(r.host.devDir, "vdb"), formatted(r), "vda has a backup GPT at its end")
}
