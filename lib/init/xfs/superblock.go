// Package xfs reads an XFS superblock, so init can find a disk by label, tell an XFS disk from a
// blank one and know how large its file system is without blkid or xfs_db.
package xfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	magic = "XFSB"

	blockSizeOffset = 4
	dataBlocksOff   = 8
	labelOffset     = 108
	labelLen        = 12
	imaxPctOffset   = 127
	superblockLen   = 128
)

// ErrNotXFS is returned for a device whose first sector holds no XFS magic.
var ErrNotXFS = errors.New("not an XFS file system")

// Superblock is the part of an XFS superblock init reads. XFS stores the fields big-endian.
type Superblock struct {
	// Label is the file system label, up to 12 bytes.
	Label string
	// BlockSize is the file system block size in bytes.
	BlockSize uint32
	// DataBlocks is the size of the data section in blocks.
	DataBlocks uint64
	// ImaxPct is the percentage of the file system that inodes may take.
	ImaxPct uint8
}

// ReadSuperblock reads the superblock of the device or image r reads. It returns ErrNotXFS when the
// magic is absent.
func ReadSuperblock(r io.ReaderAt) (Superblock, error) {
	buf := make([]byte, superblockLen)
	if _, err := r.ReadAt(buf, 0); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Superblock{}, fmt.Errorf("read superblock: %w", ErrNotXFS)
		}
		return Superblock{}, fmt.Errorf("read superblock: %w", err)
	}
	if string(buf[:len(magic)]) != magic {
		return Superblock{}, ErrNotXFS
	}
	label := buf[labelOffset : labelOffset+labelLen]
	if i := bytes.IndexByte(label, 0); i >= 0 {
		label = label[:i]
	}
	return Superblock{
		Label:      string(label),
		BlockSize:  binary.BigEndian.Uint32(buf[blockSizeOffset:]),
		DataBlocks: binary.BigEndian.Uint64(buf[dataBlocksOff:]),
		ImaxPct:    buf[imaxPctOffset],
	}, nil
}

// NewBlocks returns the data size, in blocks, a file system of this superblock grows to on a device
// of size bytes, and whether that is larger than it is now.
func (s Superblock) NewBlocks(size int64) (blocks uint64, grow bool) {
	if s.BlockSize == 0 || size <= 0 {
		return s.DataBlocks, false
	}
	blocks = uint64(size) / uint64(s.BlockSize)
	return blocks, blocks > s.DataBlocks
}
