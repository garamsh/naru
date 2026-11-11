package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/ext4"
)

const (
	configMountPoint = "/run/naru/config"
	diskPollInterval = 100 * time.Millisecond
)

// errNoDisk is returned when no block device carries the label.
var errNoDisk = errors.New("no disk with that label")

// mountFunc is syscall.Mount.
type mountFunc func(source, target, fstype string, flags uintptr, data string) error

// Host is the Node's disks and kernel settings as init reaches them.
type Host struct {
	sysClassBlock string
	devDir        string
	procSys       string
	diskWait      time.Duration
	mount         mountFunc

	dataMountPoint string
}

// NewHost returns the Host of the running machine. A disk that has not
// appeared when it is first looked for is waited for up to diskWait.
func NewHost(diskWait time.Duration) *Host {
	return &Host{
		sysClassBlock: "/sys/class/block",
		devDir:        "/dev",
		procSys:       "/proc/sys",
		diskWait:      diskWait,
		mount:         syscall.Mount,
	}
}

// ReadConfig mounts the disk labelled NARUCFG read-only and returns naru.yaml.
func (h *Host) ReadConfig() ([]byte, error) {
	dev, err := h.findByLabel(config.Label)
	if err != nil {
		return nil, fmt.Errorf("configuration disk %s: %w", config.Label, err)
	}
	if err := os.MkdirAll(configMountPoint, 0o755); err != nil {
		return nil, err
	}
	if err := h.mount(dev, configMountPoint, "ext4", syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		return nil, fmt.Errorf("mount %s: %w", dev, err)
	}
	return os.ReadFile(filepath.Join(configMountPoint, config.FileName))
}

// MountDataDisk finds the data disk, checks that it is ext4 with the verity
// feature, and mounts it. It mounts nothing from a disk that fails the check.
func (h *Host) MountDataDisk(d config.DataDisk) error {
	dev := d.Device
	if dev == "" {
		var err error
		if dev, err = h.findByLabel(d.Label); err != nil {
			return fmt.Errorf("data disk %q: %w", d.Label, err)
		}
	}
	if err := h.checkVerity(dev); err != nil {
		return fmt.Errorf("data disk %s: %w", dev, err)
	}
	if err := os.MkdirAll(d.MountPoint, 0o755); err != nil {
		return err
	}
	if err := h.mount(dev, d.MountPoint, "ext4", syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mount %s on %s: %w", dev, d.MountPoint, err)
	}
	h.dataMountPoint = d.MountPoint
	return nil
}

func (h *Host) checkVerity(dev string) error {
	f, err := os.Open(dev)
	if err != nil {
		return err
	}
	defer f.Close()
	return ext4.CheckVerity(f)
}

// EnableForwarding turns forwarding on for each family named.
func (h *Host) EnableForwarding(families []config.Family) error {
	files := map[config.Family]string{
		config.IPv4: filepath.Join(h.procSys, "net/ipv4/ip_forward"),
		config.IPv6: filepath.Join(h.procSys, "net/ipv6/conf/all/forwarding"),
	}
	for _, f := range families {
		file, ok := files[f]
		if !ok {
			return fmt.Errorf("unknown family %q", f)
		}
		if err := os.WriteFile(file, []byte("1"), 0o644); err != nil {
			return fmt.Errorf("enable %s forwarding: %w", f, err)
		}
	}
	return nil
}

// findByLabel returns the block device whose ext4 label is label, waiting up to diskWait for it to appear.
func (h *Host) findByLabel(label string) (string, error) {
	deadline := time.Now().Add(h.diskWait)
	for {
		dev, err := h.scan(label)
		if err == nil || !errors.Is(err, errNoDisk) || time.Now().After(deadline) {
			return dev, err
		}
		time.Sleep(diskPollInterval)
	}
}

// scan reads the superblock of each device in /sys/class/block.
func (h *Host) scan(label string) (string, error) {
	entries, err := os.ReadDir(h.sysClassBlock)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		dev := filepath.Join(h.devDir, e.Name())
		f, err := os.Open(dev)
		if err != nil {
			continue
		}
		sb, err := ext4.ReadSuperblock(f)
		f.Close()
		if err == nil && sb.Label == label {
			return dev, nil
		}
	}
	return "", errNoDisk
}
