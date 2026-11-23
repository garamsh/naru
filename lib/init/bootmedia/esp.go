// Package bootmedia is the extension images that came with a Node on the medium it booted from: the
// ESP of its boot disk (ADR 0096).
package bootmedia

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/garamsh/naru/lib/init/extension"
	"github.com/garamsh/naru/lib/init/upgrade"
)

// The ESP's extensions: the directory, and the index that names each file, since the ESP's files
// have 8.3 names that do not carry an extension's name.
const (
	// Dir is the directory of the ESP that holds the images and the index.
	Dir = "EXT"
	// IndexName is the index's file name in Dir.
	IndexName = "INDEX.JSN"
)

// Index is the file Dir/IndexName: the images the boot disk was built with.
type Index struct {
	Extensions []IndexEntry `json:"extensions"`
}

// IndexEntry is one image of the ESP.
type IndexEntry struct {
	// Name and Version are the manifest's, SHA256 and Bytes the image's.
	Name    string `json:"name"`
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Bytes   int64  `json:"bytes"`
	// File is the image's name in Dir.
	File string `json:"file"`
}

// ESP is the extension Source of a Node that booted from a disk image: it mounts the partition read-only
// while an image is read and unmounts it when the body is closed. A Node with no such partition, a
// microVM, has no offer from it.
type ESP struct {
	// SysClassBlock and DevDir are where the block devices are described and named, GUID the ESP's
	// partition GUID and Mountpoint where it is mounted.
	SysClassBlock, DevDir, GUID, Mountpoint string
	// Mount and Unmount are syscall.Mount and syscall.Unmount.
	Mount   func(source, target, fstype string, flags uintptr, data string) error
	Unmount func(target string, flags int) error
}

// Name says what the source is.
func (e ESP) Name() string { return "the ESP" }

// Offer returns the image of name the ESP's index lists. The partition stays mounted until the
// body is closed.
func (e ESP) Offer(name string) (extension.Offer, error) {
	device, _, err := upgrade.FindESP(e.SysClassBlock, e.DevDir, e.GUID)
	if err != nil {
		return extension.Offer{}, extension.ErrNoOffer
	}
	if err := os.MkdirAll(e.Mountpoint, 0o755); err != nil {
		return extension.Offer{}, err
	}
	if err := e.Mount(device, e.Mountpoint, "vfat", syscall.MS_RDONLY|syscall.MS_NODEV|syscall.MS_NOSUID|syscall.MS_NOEXEC, ""); err != nil {
		return extension.Offer{}, fmt.Errorf("mount the ESP %s: %w", device, err)
	}
	body, off, err := e.open(name)
	if err != nil {
		return extension.Offer{}, errors.Join(err, e.Unmount(e.Mountpoint, 0))
	}
	off.Body = &closer{ReadCloser: body, unmount: func() error { return e.Unmount(e.Mountpoint, 0) }}
	return off, nil
}

func (e ESP) open(name string) (io.ReadCloser, extension.Offer, error) {
	raw, err := os.ReadFile(filepath.Join(e.Mountpoint, Dir, IndexName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, extension.Offer{}, extension.ErrNoOffer
	}
	if err != nil {
		return nil, extension.Offer{}, err
	}
	var idx Index
	if err := json.Unmarshal(raw, &idx); err != nil {
		return nil, extension.Offer{}, fmt.Errorf("the ESP's extension index: %w", err)
	}
	for _, x := range idx.Extensions {
		if x.Name != name {
			continue
		}
		if filepath.Base(x.File) != x.File {
			return nil, extension.Offer{}, fmt.Errorf("the ESP's extension index names %q, which is not a file of %s", x.File, Dir)
		}
		f, err := os.Open(filepath.Join(e.Mountpoint, Dir, x.File))
		if err != nil {
			return nil, extension.Offer{}, err
		}
		return f, extension.Offer{SHA256: x.SHA256, Version: x.Version}, nil
	}
	return nil, extension.Offer{}, extension.ErrNoOffer
}

// closer unmounts the partition after the file is closed.
type closer struct {
	io.ReadCloser
	unmount func() error
}

func (c *closer) Close() error {
	return errors.Join(c.ReadCloser.Close(), c.unmount())
}
