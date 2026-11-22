package node

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// ext4 superblock (kernel.org ext4 disk layout): at byte 1024, the label
	// is 16 bytes at offset 120, the magic 0xEF53 at offset 56 and the
	// feature_ro_compat word at offset 100, whose bit 0x8000 is verity.
	superblock   = 1024
	labelOffset  = 120
	magicOffset  = 56
	roCompatAt   = 100
	verityBit    = 0x8000
	superblockSz = 256
)

func readSuperblock(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	sb := make([]byte, superblockSz)
	_, err = f.ReadAt(sb, superblock)
	require.NoError(t, err)
	return sb
}

// diskHolds reports which of the needles the file holds. A data disk is sparse and 8 GiB virtual, so
// the scan reads only what lseek(SEEK_DATA) says is written, a megabyte at a time, and never holds
// the disk in memory: ReadFile of it took the 16 GB of a CI runner.
func diskHolds(t *testing.T, path string, needles ...string) map[string]bool {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	found := map[string]bool{}
	keep := 0
	for _, n := range needles {
		keep = max(keep, len(n)-1)
	}
	buf := make([]byte, 1<<20)
	for off := int64(0); ; {
		start, err := f.Seek(off, 3) // SEEK_DATA
		if errors.Is(err, syscall.ENXIO) {
			break
		}
		require.NoError(t, err)
		end, err := f.Seek(start, 4) // SEEK_HOLE
		require.NoError(t, err)
		tail := []byte(nil)
		for pos := start; pos < end; {
			n, err := f.ReadAt(buf[:min(int64(len(buf)), end-pos)], pos)
			if err != nil && !errors.Is(err, io.EOF) {
				require.NoError(t, err)
			}
			window := append(tail, buf[:n]...)
			for _, needle := range needles {
				if bytes.Contains(window, []byte(needle)) {
					found[needle] = true
				}
			}
			tail = append([]byte(nil), window[max(0, len(window)-keep):]...)
			pos += int64(n)
			if n == 0 {
				break
			}
		}
		off = end
	}
	return found
}

func label(sb []byte) string {
	return strings.TrimRight(string(sb[labelOffset:labelOffset+16]), "\x00")
}

func TestBuildDataDisk_IsExt4WithVerityLabelledNARUDATA(t *testing.T) {
	data, err := BuildDataDisk(t.TempDir(), "")

	require.NoError(t, err)
	sb := readSuperblock(t, data)
	assert.Equal(t, []byte{0x53, 0xEF}, sb[magicOffset:magicOffset+2])
	assert.Equal(t, "NARUDATA", label(sb))
	roCompat := uint32(sb[roCompatAt]) | uint32(sb[roCompatAt+1])<<8 | uint32(sb[roCompatAt+2])<<16 | uint32(sb[roCompatAt+3])<<24
	assert.NotZero(t, roCompat&verityBit, "the verity feature is set")
}

func TestBuildDataDisk_CarriesTheInputsTree(t *testing.T) {
	inputs := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(inputs, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(inputs, "bin", "tool"), []byte("marker-inputs-9f3a"), 0o755))

	data, err := BuildDataDisk(t.TempDir(), inputs)

	require.NoError(t, err)
	assert.True(t, diskHolds(t, data, "marker-inputs-9f3a")["marker-inputs-9f3a"])
}

func TestBuildDataDiskWith_CarriesAKeptAgentPairWhereTheAgentKeepsIt(t *testing.T) {
	pair := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(pair, "node-agent.pem"), []byte("marker-pair-4c1e"), 0o600))

	data, err := BuildDataDiskWith(t.TempDir(), "", pair, "")

	require.NoError(t, err)
	held := diskHolds(t, data, "marker-pair-4c1e", "gaon-node-agent")
	assert.True(t, held["marker-pair-4c1e"])
	assert.True(t, held["gaon-node-agent"])
}

func TestBuildDataDiskWith_CarriesTheExtensionStoreWhereInitKeepsItsExtensions(t *testing.T) {
	store := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(store, strings.Repeat("a", 64)+".sqfs"), []byte("marker-image-77d0"), 0o600))

	data, err := BuildDataDiskWith(t.TempDir(), "", "", store)

	require.NoError(t, err)
	held := diskHolds(t, data, "marker-image-77d0", strings.Repeat("a", 64)+".sqfs")
	assert.True(t, held["marker-image-77d0"])
	assert.True(t, held[strings.Repeat("a", 64)+".sqfs"])
}

func TestBuildCidata_IsA4MiBVolumeLabelledCidataHoldingTheThreeFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cidata.img")

	require.NoError(t, BuildCidata(path, "naru15-node-1", []byte("marker-user-data"), []byte("marker-network-config")))

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Len(t, b, 4<<20)
	assert.Equal(t, "CD001", string(b[16*2048+1:16*2048+6]))
	assert.Equal(t, "cidata", strings.TrimSpace(string(b[16*2048+40:16*2048+72])))
	for _, want := range []string{"marker-user-data", "marker-network-config", "instance-id: naru15-node-1"} {
		assert.Contains(t, string(b), want)
	}
}

func TestHasLine(t *testing.T) {
	console := "boot\nlevel=INFO msg=\"configuration accepted\" node=a\nservice=mvmd msg=started\n"

	assert.True(t, hasLine(console, []string{"configuration accepted", "node=a"}))
	assert.True(t, hasLine(console, []string{"service=mvmd", "started"}))
	assert.False(t, hasLine(console, []string{"started", "service=mvmd"}), "fragments must come in order")
	assert.False(t, hasLine(console, []string{"configuration accepted", "started"}), "fragments must share a line")
}

func argsOf(t *testing.T, spec Spec) string {
	t.Helper()
	spec.Dir = t.TempDir()
	spec.Disk = filepath.Join(t.TempDir(), "naru.img")
	require.NoError(t, os.WriteFile(spec.Disk, []byte("disk"), 0o644))
	spec.Mcast = "230.78.0.1:5000"
	spec.Config = Config{Name: "node-1", Index: 1, Total: 2}
	args, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")
	require.NoError(t, err)
	return strings.Join(args, " ")
}

func TestQemuArgs_TheNodeBootsItsDiskUnderOVMFWithNoKernelInitrdAppendOrIP(t *testing.T) {
	line := argsOf(t, Spec{})

	for _, flag := range []string{" -kernel", " -initrd", " -append", "ip="} {
		assert.NotContains(t, line, flag)
	}
	assert.Contains(t, line, "unit=0,readonly=on,file="+OVMFCode, "the firmware code is read-only")
	assert.Contains(t, line, "naru.img", "the disk is attached")
	assert.Contains(t, line, "scsi-hd,bus=scsihw0.0,drive=boot,bootindex=0")
	assert.Contains(t, line, "file=data.img", "the data disk is attached")
}

func TestQemuArgs_TheNoCloudDriveIsAReadOnlyCDROMOnTheAHCIBus(t *testing.T) {
	line := argsOf(t, Spec{})

	assert.Contains(t, line, "media=cdrom,readonly=on,file=cidata.img")
	assert.Contains(t, line, "ide-cd,bus=ide.1,drive=cidata")
	assert.Contains(t, line, "-machine q35")
	assert.Contains(t, line, "virtio-scsi-pci")
}

func TestQemuArgs_ANodeGetsItsOwnCopyOfTheOVMFVariables(t *testing.T) {
	line := argsOf(t, Spec{})

	assert.Contains(t, line, "unit=1,file=")
	assert.NotContains(t, line, "unit=1,file="+OVMFVars, "the shared variable store is never opened for writing")
}

func TestQemuArgs_AMissingDiskFailsNamingItBeforeQEMUStarts(t *testing.T) {
	spec := Spec{Disk: filepath.Join(t.TempDir(), "naru.img"), Dir: t.TempDir(), Mcast: "230.78.0.1:5000", Config: Config{Name: "node-1", Index: 1, Total: 2}}

	_, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "naru.img")
	assert.Contains(t, err.Error(), "make disk")
}
