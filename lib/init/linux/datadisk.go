package linux

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/ext4"
	"github.com/garamsh/naru/lib/init/upgrade"
	"github.com/garamsh/naru/lib/wire"
)

// FormatBlankDataDisk makes the data disk d names when spec.dataDisk.formatBlank asks for it: if
// no disk is ext4 and labelled d.Label, it formats the one blank disk (isBlankDisk: first and last MiB all zeros) as
// ext4 with the verity feature and that label (ADR 0003's data disk), and logs it. A disk that
// holds anything is never formatted, and a disk labelled d.Label is left as it is. More than one
// blank disk is an error: init does not guess which is the data disk. With d.FirstBlank (the join
// form) it takes the first blank whole disk instead, never the one the ESP init booted from, and
// refuses the boot when there is none.
func (h *Host) FormatBlankDataDisk(d config.DataDisk, log *slog.Logger) error {
	dev, err := h.wait(func() (string, error) { return h.blankOrLabelled(d.Label, d.FirstBlank) })
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
	if out, err := h.run(wire.MkfsPath, "-q", "-F", "-b", "4096", "-O", "verity", "-L", d.Label, dev); err != nil {
		return fmt.Errorf("data disk %q: mkfs.ext4 %s: %w: %s", d.Label, dev, err, strings.TrimSpace(string(out)))
	}
	log.Info("data disk formatted as ext4", "label", d.Label, "device", dev)
	return nil
}

// blankOrLabelled returns the device that holds an ext4 file system labelled label or, when there
// is none, the only whole disk that is blank, or the first one when first is set and the disk is
// not the boot disk. It returns errNoDisk when there is neither.
func (h *Host) blankOrLabelled(label string, first bool) (string, error) {
	if dev, err := h.scan(ext4Label, func(got string) bool { return got == label }); err == nil {
		return dev, nil
	}
	entries, err := os.ReadDir(h.sysClassBlock)
	if err != nil {
		return "", err
	}
	boot := ""
	if first {
		boot = h.bootDisk()
	}
	var blank []string
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(h.sysClassBlock, e.Name(), "partition")); err == nil || e.Name() == boot {
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
	if first && len(blank) > 0 {
		return blank[0], nil
	}
	switch len(blank) {
	case 0:
		return "", fmt.Errorf("no ext4 disk labelled %q and no blank disk: %w", label, errNoDisk)
	case 1:
		return blank[0], nil
	}
	return "", fmt.Errorf("%d blank disks (%s) and none labelled %q: not choosing one", len(blank), strings.Join(blank, ", "), label)
}

// bootDisk returns the name in /sys/class/block of the disk that holds the partition whose
// PARTUUID is wire.ESPGUID, and "" when no partition has it.
func (h *Host) bootDisk() string {
	entries, err := os.ReadDir(h.sysClassBlock)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		dir := filepath.Join(h.sysClassBlock, e.Name())
		uevent, err := os.ReadFile(filepath.Join(dir, "uevent"))
		if err != nil || !upgrade.HasPartUUID(string(uevent), wire.ESPGUID) {
			continue
		}
		// A partition's sysfs entry lies under its disk's.
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		return filepath.Base(filepath.Dir(real))
	}
	return ""
}
