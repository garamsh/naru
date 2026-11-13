// Package storageprobe is the service naru's QEMU tests run on a Node with a storage disk: it
// reports what the disk mounted at a directory is, whether it reflinks, and whether a marker
// written on an earlier boot is still there.
package storageprobe

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

const (
	xfsMagic  = 0x58465342
	ficlone   = 0x40049409 // FICLONE, _IOW(0x94, 9, int)
	markerTxt = "naru66"
)

// Run reports on the file system mounted at dir through one log line, "storageprobe", whose
// fields are fs (xfs or the magic), reflink (ok or the failure), marker (written, or kept when an
// earlier boot wrote it) and size (the bytes of the file system).
func Run(dir string, log *slog.Logger) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return err
	}
	fs := "xfs"
	if st.Type != xfsMagic {
		fs = fmt.Sprintf("%#x", st.Type)
	}
	marker, err := mark(filepath.Join(dir, "marker"))
	if err != nil {
		return err
	}
	reflink := "ok"
	if err := reflinkCopy(dir); err != nil {
		reflink = "failed: " + err.Error()
	}
	log.Info("storageprobe", "fs", fs, "reflink", reflink, "marker", marker, "size", uint64(st.Blocks)*uint64(st.Bsize)) //nolint:gosec // a size
	return nil
}

// mark writes the marker file and reports "written", or reports "kept" when it holds the marker.
func mark(path string) (string, error) {
	got, err := os.ReadFile(path)
	switch {
	case err == nil && string(got) == markerTxt:
		return "kept", nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(markerTxt); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return "written", syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// reflinkCopy writes a file, clones it with FICLONE into another and checks the clone holds the
// same bytes.
func reflinkCopy(dir string) error {
	src, dst := filepath.Join(dir, "clone-src"), filepath.Join(dir, "clone-dst")
	data := bytes.Repeat([]byte("naru66-reflink-"), 1<<16)
	if err := os.WriteFile(src, data, 0o600); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, out.Fd(), ficlone, in.Fd()); errno != 0 {
		return errno
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return err
	}
	got, err := io.ReadAll(out)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, data) {
		return errors.New("the clone differs from its source")
	}
	return nil
}
