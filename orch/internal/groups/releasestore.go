package groups

import (
	"context"
	"io"
)

// BootSet is a boot set of a release the store has verified: its name and the digests of its
// two files, and a way to read them.
type BootSet struct {
	Name            string
	KernelSHA256    string
	InitramfsSHA256 string
	// Open starts the reads of the kernel and the initramfs. A read whose bytes are not the ones
	// the digest names ends in an error and not in io.EOF.
	Open func(ctx context.Context) (kernel, initramfs io.ReadCloser, err error)
}

// ReleaseStore is where the release assets of the Node versions are. Fetch verifies the release of
// version against its signature and returns its boot set; it returns an error, and no boot set,
// for a release that does not verify.
type ReleaseStore interface {
	Fetch(ctx context.Context, version string) (BootSet, error)
}
