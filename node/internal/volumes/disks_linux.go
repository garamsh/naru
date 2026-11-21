package volumes

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// SysDisks finds a disk by serial number in the kernel's block device directory.
type SysDisks struct {
	// Sys is /sys/class/block and Dev /dev; the zero value is those.
	Sys, Dev string
	// MountInfo is /proc/self/mountinfo.
	MountInfo string
}

// Find returns the one whole disk whose serial number is serial, as a virtio-blk disk gives it in
// serial, or a SCSI disk in its wwid. A partition has no serial of its own.
func (s SysDisks) Find(serial string) (Disk, error) {
	sys, dev := s.Sys, s.Dev
	if sys == "" {
		sys = "/sys/class/block"
	}
	if dev == "" {
		dev = "/dev"
	}
	entries, err := os.ReadDir(sys)
	if err != nil {
		return Disk{}, err
	}
	var found []Disk
	for _, e := range entries {
		dir := filepath.Join(sys, e.Name())
		if !hasSerial(dir, serial) {
			continue
		}
		sectors, err := os.ReadFile(filepath.Join(dir, "size"))
		if err != nil {
			return Disk{}, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(sectors)), 10, 64)
		if err != nil {
			return Disk{}, fmt.Errorf("%s/size: %w", dir, err)
		}
		found = append(found, Disk{Path: filepath.Join(dev, e.Name()), Bytes: n * 512})
	}
	switch len(found) {
	case 0:
		return Disk{}, fmt.Errorf("no disk has the serial %q", serial)
	case 1:
		return found[0], nil
	}
	return Disk{}, fmt.Errorf("%d disks have the serial %q", len(found), serial)
}

func hasSerial(dir, serial string) bool {
	for _, name := range []string{"serial", "device/wwid"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil && strings.Trim(string(b), " \t\r\n\x00") == serial {
			return true
		}
	}
	return false
}

// InUse returns an error when the Node itself uses the disk: it, or a partition of it, is mounted
// (the boot disk's /var/lib/naru and EFI system partition are such mounts), or a device is mapped
// over it, which the disk's holders directory lists.
func (s SysDisks) InUse(disk Disk) error {
	sys, mounts := s.Sys, s.MountInfo
	if sys == "" {
		sys = "/sys/class/block"
	}
	if mounts == "" {
		mounts = "/proc/self/mountinfo"
	}
	name := filepath.Base(disk.Path)
	parts := []string{name}
	entries, err := os.ReadDir(filepath.Join(sys, name))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), name) && e.IsDir() {
			parts = append(parts, e.Name())
		}
	}
	for _, p := range parts {
		dir := filepath.Join(sys, name)
		if p != name {
			dir = filepath.Join(dir, p)
		}
		if held, err := os.ReadDir(filepath.Join(dir, "holders")); err == nil && len(held) > 0 {
			return fmt.Errorf("%s is under the device %s", p, held[0].Name())
		}
	}
	mounted, err := readMountInfoFile(mounts)
	if err != nil {
		return err
	}
	for _, m := range mounted {
		if slices.Contains(parts, filepath.Base(m.source)) && strings.HasPrefix(m.source, "/dev/") {
			return fmt.Errorf("%s is mounted at %s", m.source, m.point)
		}
	}
	return nil
}
