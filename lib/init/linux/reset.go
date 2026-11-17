package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/store"
)

// ClearDataDisk removes everything on the mounted data disk except the stored configuration and
// what the file system itself keeps, which is the reset of scope data (ADR 0082). It goes on past
// an entry it cannot remove and returns the first such error.
func (h *Host) ClearDataDisk() error {
	if h.dataMountPoint == "" {
		return errors.New("the data disk is not mounted")
	}
	entries, err := os.ReadDir(h.dataMountPoint)
	if err != nil {
		return err
	}
	var first error
	for _, e := range entries {
		if n := e.Name(); n == filepath.Base(store.Dir) || n == "lost+found" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(h.dataMountPoint, e.Name())); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// WipeDataDisk unmounts the data disk and blanks it: its first and its last blankProbe bytes become
// zeros, which is what leaves no file system signature and no partition table for the format of a
// blank disk to find (ADR 0070). It is the reset of scope all (ADR 0082).
func (h *Host) WipeDataDisk(d config.DataDisk) error {
	dev := d.Device
	if dev == "" {
		var err error
		if dev, err = h.scan(ext4Label, func(got string) bool { return got == d.Label }); err != nil {
			return fmt.Errorf("data disk %q: %w", d.Label, err)
		}
	}
	unmount := h.unmount
	if unmount == nil {
		unmount = func(dir string) error { return syscall.Unmount(dir, 0) }
	}
	if h.dataMountPoint != "" {
		if err := unmount(h.dataMountPoint); err != nil {
			return fmt.Errorf("unmount %s: %w", h.dataMountPoint, err)
		}
		h.dataMountPoint = ""
	}
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	size, err := f.Seek(0, 2)
	if err != nil {
		return err
	}
	zero := make([]byte, blankProbe)
	if _, err := f.WriteAt(zero[:min(size, blankProbe)], 0); err != nil {
		return fmt.Errorf("blank %s: %w", dev, err)
	}
	if size > blankProbe {
		if _, err := f.WriteAt(zero, size-blankProbe); err != nil {
			return fmt.Errorf("blank %s: %w", dev, err)
		}
	}
	return f.Sync()
}
