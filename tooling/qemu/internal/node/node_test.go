package node

import (
	"os"
	"path/filepath"
	"strings"
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
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b[superblock : superblock+superblockSz]
}

func label(sb []byte) string {
	return strings.TrimRight(string(sb[labelOffset:labelOffset+16]), "\x00")
}

func TestBuildDisks_ConfigDiskIsExt4LabelledNARUCFG(t *testing.T) {
	config, _, err := BuildDisks(t.TempDir(), []byte("x"), []byte("{}"), "")

	require.NoError(t, err)
	sb := readSuperblock(t, config)
	assert.Equal(t, []byte{0x53, 0xEF}, sb[magicOffset:magicOffset+2])
	assert.Equal(t, "NARUCFG", label(sb))
}

func TestBuildDisks_DataDiskIsExt4WithVerityLabelledNARUDATA(t *testing.T) {
	_, data, err := BuildDisks(t.TempDir(), []byte("x"), []byte("{}"), "")

	require.NoError(t, err)
	sb := readSuperblock(t, data)
	assert.Equal(t, "NARUDATA", label(sb))
	roCompat := uint32(sb[roCompatAt]) | uint32(sb[roCompatAt+1])<<8 | uint32(sb[roCompatAt+2])<<16 | uint32(sb[roCompatAt+3])<<24
	assert.NotZero(t, roCompat&verityBit, "the verity feature is set")
}

func TestBuildDisks_ConfigDiskHoldsTheGivenNaruYAML(t *testing.T) {
	config, _, err := BuildDisks(t.TempDir(), []byte("apiVersion: marker-xyz\n"), []byte("{}"), "")

	require.NoError(t, err)
	b, err := os.ReadFile(config)
	require.NoError(t, err)
	assert.Contains(t, string(b), "apiVersion: marker-xyz")
}

func TestBuildDisks_DataDiskCarriesTheInputsTree(t *testing.T) {
	inputs := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(inputs, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(inputs, "bin", "tool"), []byte("marker-inputs-9f3a"), 0o755))

	_, data, err := BuildDisks(t.TempDir(), []byte("x"), []byte("{}"), inputs)

	require.NoError(t, err)
	b, err := os.ReadFile(data)
	require.NoError(t, err)
	assert.Contains(t, string(b), "marker-inputs-9f3a")
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
	if spec.Disk != "" {
		spec.Disk = filepath.Join(t.TempDir(), "naru.img")
		require.NoError(t, os.WriteFile(spec.Disk, []byte("disk"), 0o644))
	}
	spec.Mcast = "230.78.0.1:5000"
	spec.Config = Config{Name: "node-1", Index: 1, Total: 2}
	args, err := qemuArgs(spec, "naru15-node-1", "cfg.img", "data.img", "console.log")
	require.NoError(t, err)
	return strings.Join(args, " ")
}

func TestQemuArgs_ADiskBootsUnderOVMFWithNoKernelInitrdOrAppend(t *testing.T) {
	line := argsOf(t, Spec{Disk: "naru.img"})

	for _, flag := range []string{" -kernel", " -initrd", " -append"} {
		assert.NotContains(t, line, flag)
	}
	assert.Contains(t, line, "unit=0,readonly=on,file="+OVMFCode, "the firmware code is read-only")
	assert.Contains(t, line, "naru.img", "the disk is attached")
	assert.Contains(t, line, "bootindex=0")
	assert.Contains(t, line, "file=cfg.img", "the configuration disk is attached")
	assert.Contains(t, line, "file=data.img", "the data disk is attached")
}

func TestQemuArgs_ANodeGetsItsOwnCopyOfTheOVMFVariables(t *testing.T) {
	line := argsOf(t, Spec{Disk: "naru.img"})

	assert.Contains(t, line, "unit=1,file=")
	assert.NotContains(t, line, "unit=1,file="+OVMFVars, "the shared variable store is never opened for writing")
}

func TestQemuArgs_AKernelBootsDirectlyAndCarriesTheAddress(t *testing.T) {
	line := argsOf(t, Spec{Kernel: "bzImage", Initrd: "initramfs.cpio.gz"})

	assert.Contains(t, line, "-kernel bzImage -initrd initramfs.cpio.gz")
	assert.Contains(t, line, "ip=10.78.0.1:::255.255.255.0:node-1:eth0:off")
	assert.Contains(t, line, "loglevel=4")
	assert.NotContains(t, line, "pflash", "no firmware on the direct-boot path")
}

func TestQemuArgs_AMissingDiskFailsNamingItBeforeQEMUStarts(t *testing.T) {
	spec := Spec{Disk: filepath.Join(t.TempDir(), "naru.img"), Dir: t.TempDir(), Mcast: "230.78.0.1:5000", Config: Config{Name: "node-1", Index: 1, Total: 2}}

	_, err := qemuArgs(spec, "naru15-node-1", "cfg.img", "data.img", "console.log")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "naru.img")
	assert.Contains(t, err.Error(), "make disk")
}
