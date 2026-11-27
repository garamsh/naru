package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/garamsh/naru/host/internal/mvmd"
)

// The fake's disk stream, as mvmd's: a disk's bytes are f.data's, zero where it holds none, the
// stream one extent of them, and the digest over the disk's whole size.

func (f *fakeRuntime) diskBytes(id string) []byte {
	b := make([]byte, f.disks[id].SizeBytes)
	copy(b, f.data[id])
	return b
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ReadDisk refuses a disk a running or paused Machine holds, as mvmd does.
func (f *fakeRuntime) ReadDisk(ctx context.Context, id string, sink func(mvmd.DiskPart) error) error {
	f.mu.Lock()
	if err := f.step("readDisk " + id); err != nil {
		f.mu.Unlock()
		return err
	}
	d, ok := f.disks[id]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("disk %s: %w", id, mvmd.ErrNotFound)
	}
	for _, m := range f.machines {
		held := slices.ContainsFunc(m.Disks, func(a mvmd.Attachment) bool { return a.ID == id })
		if held && m.State != mvmd.StateCreated && m.State != mvmd.StateStopped {
			f.mu.Unlock()
			return fmt.Errorf("%w: disk %s is held by the %s Machine %s", mvmd.ErrPrecondition, id, m.State, m.ID)
		}
	}
	b, fs, n, corrupt, truncate := f.diskBytes(id), f.filesystems[id], len(f.data[id]), f.corrupt[id], f.truncate[id]
	stall, stalls := f.stallAt[id]
	if fs == "" {
		fs = mvmd.FilesystemExt4
	}
	f.mu.Unlock()
	parts := []mvmd.DiskPart{mvmd.DiskHeader{SizeBytes: d.SizeBytes, Filesystem: fs}}
	digest := digestOf(b)
	if n > 0 {
		data := b[:n]
		if corrupt {
			data = append([]byte{data[0] ^ 0xff}, data[1:]...)
		}
		parts = append(parts, mvmd.DiskExtent{Offset: 0, Data: data})
	}
	if !truncate {
		parts = append(parts, mvmd.DiskDigest(digest))
	}
	for i, p := range parts {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stalls && i == stall {
			<-ctx.Done()
			return ctx.Err()
		}
		if err := sink(p); err != nil {
			return err
		}
	}
	return nil
}

// CreateDiskFromStream publishes the disk only when the extents hash to the digest that ends the
// stream; the same ID with the same digest again is success, and with another a conflict.
func (f *fakeRuntime) CreateDiskFromStream(_ context.Context, id string, h mvmd.DiskHeader, parts func(send func(mvmd.DiskPart) error) error) error {
	f.mu.Lock()
	err := f.step("createDiskFromStream " + id)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	b := make([]byte, h.SizeBytes)
	digest := ""
	end := uint64(0)
	if err := parts(func(p mvmd.DiskPart) error {
		if digest != "" {
			return fmt.Errorf("%w: a message after the digest", mvmd.ErrInvalid)
		}
		switch e := p.(type) {
		case mvmd.DiskExtent:
			if e.Offset < end || e.Offset+uint64(len(e.Data)) > h.SizeBytes {
				return fmt.Errorf("%w: an extent out of order or past the size", mvmd.ErrInvalid)
			}
			copy(b[e.Offset:], e.Data)
			end = e.Offset + uint64(len(e.Data))
		case mvmd.DiskDigest:
			digest = string(e)
		default:
			return fmt.Errorf("%w: a part after the start that is no extent or digest", mvmd.ErrInvalid)
		}
		return nil
	}); err != nil {
		return err
	}
	switch {
	case digest == "":
		return fmt.Errorf("%w: a stream without a digest", mvmd.ErrInvalid)
	case digestOf(b) != digest:
		return fmt.Errorf("%w: the bytes hash to %s, not %s", mvmd.ErrInvalid, digestOf(b), digest)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.disks[id]; ok {
		if f.digests[id] != digest {
			return fmt.Errorf("%w: disk %s holds other bytes", mvmd.ErrConflict, id)
		}
		return nil
	}
	f.disks[id] = mvmd.Disk{ID: id, SizeBytes: h.SizeBytes}
	f.data[id], f.filesystems[id], f.digests[id] = b[:end], h.Filesystem, digest
	return nil
}
