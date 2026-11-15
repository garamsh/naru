// Package atomicfile replaces a file so that a reader, and a crash, sees the old file or the new
// one and never a part of it.
package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
)

// TempPrefix starts the name of the temporary file Write makes, which a crash can leave behind.
const TempPrefix = ".atomic-"

// Option adjusts the file Write makes.
type Option func(*options)

type options struct {
	owner *[2]int
}

// Owner makes the file owned by uid and gid before it is put in place.
func Owner(uid, gid int) Option {
	return func(o *options) { o.owner = &[2]int{uid, gid} }
}

// Write replaces the file at path with data, of mode perm. It writes a temporary file in the
// directory of path, syncs it, renames it over path and syncs the directory, so that once Write
// returns the file survives a crash and until then path holds the old file or none. The
// directory of path must exist. A failure leaves path as it was and no temporary file behind.
func Write(path string, data []byte, perm os.FileMode, opts ...Option) (err error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil && o.owner != nil {
		err = f.Chown(o.owner[0], o.owner[1])
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return SyncDir(dir)
}

// SyncDir makes the entries of the directory at dir durable.
func SyncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // the directory of a file the caller writes
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
