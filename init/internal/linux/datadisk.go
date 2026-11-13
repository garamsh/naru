package linux

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/ext4"
)

// mkfsExt4 is the mke2fs the image carries as mkfs.ext4 (ADR 0011).
const mkfsExt4 = "/bin/mkfs.ext4"

// FormatBlankDataDisk makes the data disk d names when spec.dataDisk.formatBlank asks for it: if
// no disk is ext4 and labelled d.Label, it formats the one disk whose first MiB is all zeros as
// ext4 with the verity feature and that label (ADR 0003's data disk), and logs it. A disk that
// holds anything is never formatted, and a disk labelled d.Label is left as it is. More than one
// blank disk is an error: init does not guess which is the data disk.
func (h *Host) FormatBlankDataDisk(d config.DataDisk, log *slog.Logger) error {
	dev, err := h.wait(func() (string, error) { return h.blankOrLabelled(d.Label) })
	if err != nil {
		return fmt.Errorf("data disk %q: %w", d.Label, err)
	}
	f, err := os.Open(dev)
	if err != nil {
		return fmt.Errorf("data disk %q: %w", d.Label, err)
	}
	sb, serr := ext4.ReadSuperblock(f)
	f.Close()
	if serr == nil && sb.Label == d.Label {
		return nil
	}
	// The block size is the page size, which verity needs.
	if out, err := h.run(mkfsExt4, "-q", "-F", "-b", "4096", "-O", "verity", "-L", d.Label, dev); err != nil {
		return fmt.Errorf("data disk %q: mkfs.ext4 %s: %w: %s", d.Label, dev, err, strings.TrimSpace(string(out)))
	}
	log.Info("data disk formatted as ext4", "label", d.Label, "device", dev)
	return nil
}

// blankOrLabelled returns the device that holds an ext4 file system labelled label or, when there
// is none, the only whole disk that is blank. It returns errNoDisk when there is neither.
func (h *Host) blankOrLabelled(label string) (string, error) {
	if dev, err := h.scan(ext4Label, func(got string) bool { return got == label }); err == nil {
		return dev, nil
	}
	entries, err := os.ReadDir(h.sysClassBlock)
	if err != nil {
		return "", err
	}
	var blank []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(h.sysClassBlock, e.Name(), "partition")); err == nil {
			continue
		}
		dev := filepath.Join(h.devDir, e.Name())
		f, err := os.Open(dev)
		if err != nil {
			continue
		}
		ok, err := isBlankDisk(f)
		f.Close()
		if err == nil && ok {
			blank = append(blank, dev)
		}
	}
	switch len(blank) {
	case 0:
		return "", fmt.Errorf("no ext4 disk labelled %q and no blank disk: %w", label, errNoDisk)
	case 1:
		return blank[0], nil
	}
	return "", fmt.Errorf("%d blank disks (%s) and none labelled %q: not choosing one", len(blank), strings.Join(blank, ", "), label)
}

// isBlankDisk reports whether f holds at least one byte and its first blankProbe bytes are zeros.
func isBlankDisk(f *os.File) (bool, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil || size == 0 {
		return false, err
	}
	return isBlank(f)
}
