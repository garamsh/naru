package xfs_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/xfs"
)

// image returns the first sector of an XFS file system as mkfs.xfs lays it out.
func image(label string, blockSize uint32, dataBlocks uint64, imaxPct uint8) []byte {
	b := make([]byte, 512)
	copy(b, "XFSB")
	binary.BigEndian.PutUint32(b[4:], blockSize)
	binary.BigEndian.PutUint64(b[8:], dataBlocks)
	copy(b[108:120], label)
	b[127] = imaxPct
	return b
}

func TestReadSuperblock_ReadsTheFieldsBigEndian(t *testing.T) {
	sb, err := xfs.ReadSuperblock(bytes.NewReader(image("vmdisks", 4096, 131072, 25)))

	require.NoError(t, err)
	assert.Equal(t, xfs.Superblock{Label: "vmdisks", BlockSize: 4096, DataBlocks: 131072, ImaxPct: 25}, sb)
}

func TestReadSuperblock_ReadsATwelveByteLabelWithNoTerminator(t *testing.T) {
	sb, err := xfs.ReadSuperblock(bytes.NewReader(image("abcdefghijkl", 4096, 1, 25)))

	require.NoError(t, err)
	assert.Equal(t, "abcdefghijkl", sb.Label)
}

func TestReadSuperblock_RefusesWhatIsNotXFS(t *testing.T) {
	tests := map[string][]byte{
		"zeros":     make([]byte, 512),
		"too short": []byte("XF"),
		"ext4":      append(make([]byte, 1024), 0x53, 0xEF),
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := xfs.ReadSuperblock(bytes.NewReader(data))

			assert.ErrorIs(t, err, xfs.ErrNotXFS)
		})
	}
}

func TestSuperblock_NewBlocksGrowsOnlyOnALargerDevice(t *testing.T) {
	sb := xfs.Superblock{BlockSize: 4096, DataBlocks: 1000}
	tests := []struct {
		name   string
		size   int64
		blocks uint64
		grow   bool
	}{
		{"a larger device", 2000*4096 + 100, 2000, true},
		{"the same size", 1000 * 4096, 1000, false},
		{"a device a partial block larger", 1000*4096 + 4095, 1000, false},
		{"a smaller device", 500 * 4096, 500, false},
		{"an unknown size", 0, 1000, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks, grow := sb.NewBlocks(tt.size)

			assert.Equal(t, tt.grow, grow)
			if tt.grow {
				assert.Equal(t, tt.blocks, blocks)
			}
		})
	}
}
