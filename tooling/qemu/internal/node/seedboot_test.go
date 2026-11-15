package node

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNocloudDrive_ASeedIsTheDriveAsItIsAndWritesNoVolumeOfItsOwn(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(t.TempDir(), "seed.iso")
	require.NoError(t, os.WriteFile(seed, []byte("an installation seed"), 0o644))

	drive, err := nocloudDrive(Spec{Seed: seed, Dir: dir, Config: Config{Name: "joined-1", Index: 4, Total: 5}})

	require.NoError(t, err)
	assert.Equal(t, seed, drive, "the same file, so that every host of the fleet boots from the same bytes")
	_, err = os.Stat(filepath.Join(dir, "cidata.img"))
	assert.True(t, os.IsNotExist(err), "no user-data or network-config is rendered beside the seed")
}

func TestNocloudDrive_AMissingSeedIsRefusedNamingIt(t *testing.T) {
	_, err := nocloudDrive(Spec{Seed: filepath.Join(t.TempDir(), "absent.iso"), Dir: t.TempDir()})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent.iso")
}

func TestNocloudDrive_WithoutASeedIsTheRenderedVolumeAsBefore(t *testing.T) {
	dir := t.TempDir()

	drive, err := nocloudDrive(Spec{Dir: dir, NaruYAML: []byte("marker-user-data"), NetworkConfig: []byte("marker-network-config"), Config: Config{Name: "node-1", Index: 1, Total: 2}})

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "cidata.img"), drive)
	b, err := os.ReadFile(drive)
	require.NoError(t, err)
	assert.Contains(t, string(b), "marker-user-data")
}

func TestDataDiskOf_ABlankDiskHasTheSizeOfADataDiskAndNoFileSystem(t *testing.T) {
	data, err := dataDiskOf(Spec{Dir: t.TempDir(), BlankData: true})

	require.NoError(t, err)
	st, err := os.Stat(data)
	require.NoError(t, err)
	assert.Equal(t, int64(DataDiskBytes), st.Size())
	f, err := os.Open(data)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	sb := make([]byte, 4096)
	_, err = f.ReadAt(sb, 0)
	require.NoError(t, err)
	assert.Equal(t, make([]byte, 4096), sb, "nothing is written: no ext4 superblock, no NARUDATA label")
}
