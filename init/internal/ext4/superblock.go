// Package ext4 reads an ext4 superblock, so init can find a disk by label and
// check a disk's features without blkid.
package ext4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	superblockOffset = 1024
	superblockSize   = 1024

	magic          = 0xEF53
	magicOffset    = 0x38
	roCompatOffset = 0x64
	labelOffset    = 0x78
	labelLen       = 16
	roCompatVerity = 0x8000
)

// ErrNotExt4 is returned for a device whose superblock holds no ext2/3/4 magic.
var ErrNotExt4 = errors.New("not an ext4 file system")

// ErrNoVerity is returned by CheckVerity for a file system without the verity feature.
var ErrNoVerity = errors.New("ext4 file system lacks the verity feature")

// Superblock is the part of an ext4 superblock init reads.
type Superblock struct {
	// Label is the volume label, up to 16 bytes.
	Label string
	// roCompat is s_feature_ro_compat.
	roCompat uint32
}

// Verity reports whether the file system has the verity feature, which
// fs-verity needs.
func (s Superblock) Verity() bool {
	return s.roCompat&roCompatVerity != 0
}

// ReadSuperblock reads the superblock from the device or image r reads. It
// returns ErrNotExt4 when the magic is absent.
func ReadSuperblock(r io.ReaderAt) (Superblock, error) {
	buf := make([]byte, superblockSize)
	if _, err := r.ReadAt(buf, superblockOffset); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return Superblock{}, fmt.Errorf("read superblock: %w", ErrNotExt4)
		}
		return Superblock{}, fmt.Errorf("read superblock: %w", err)
	}
	if binary.LittleEndian.Uint16(buf[magicOffset:]) != magic {
		return Superblock{}, ErrNotExt4
	}
	label := buf[labelOffset : labelOffset+labelLen]
	if i := bytes.IndexByte(label, 0); i >= 0 {
		label = label[:i]
	}
	return Superblock{
		Label:    string(label),
		roCompat: binary.LittleEndian.Uint32(buf[roCompatOffset:]),
	}, nil
}

// CheckVerity returns nil when r holds an ext4 file system with the verity
// feature, ErrNotExt4 or ErrNoVerity otherwise.
func CheckVerity(r io.ReaderAt) error {
	sb, err := ReadSuperblock(r)
	if err != nil {
		return err
	}
	if !sb.Verity() {
		return ErrNoVerity
	}
	return nil
}
