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
