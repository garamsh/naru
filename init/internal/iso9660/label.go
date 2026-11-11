// Package iso9660 reads the volume label of an ISO 9660 file system, which is
// all init needs to know of one before the kernel mounts it.
package iso9660

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// descriptorsAt is where the volume descriptors start: sector 16 of 2048 bytes.
	descriptorsAt = 16 * 2048
	pvdType       = 1
	labelAt       = 40
	labelLen      = 32
)

// ErrNotISO9660 is returned when the device has no primary volume descriptor.
var ErrNotISO9660 = errors.New("not an ISO 9660 file system")

// ReadLabel returns the volume identifier of the primary volume descriptor, without its padding.
func ReadLabel(r io.ReaderAt) (string, error) {
	var pvd [labelAt + labelLen]byte
	if _, err := r.ReadAt(pvd[:], descriptorsAt); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", ErrNotISO9660
		}
		return "", fmt.Errorf("read the volume descriptor: %w", err)
	}
	if pvd[0] != pvdType || string(pvd[1:6]) != "CD001" || pvd[6] != 1 {
		return "", ErrNotISO9660
	}
	return strings.TrimRight(string(pvd[labelAt:]), " "), nil
}
