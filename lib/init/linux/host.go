package linux

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/garamsh/naru/lib/init/boot"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/dhcp4"
	"github.com/garamsh/naru/lib/init/ext4"
	"github.com/garamsh/naru/lib/init/iso9660"
	"github.com/garamsh/naru/lib/init/keptpair"
	"github.com/garamsh/naru/lib/init/netcfg"
	"github.com/garamsh/naru/lib/init/registry"
	"github.com/garamsh/naru/lib/init/timesync"
	"github.com/garamsh/naru/lib/wire"
)

const (
	seedMountPoint   = "/run/naru/cidata"
	diskPollInterval = 100 * time.Millisecond
	// carrierWait is how long a Node with no network-config waits for an Ethernet link to see a
	// carrier.
	carrierWait = 30 * time.Second
)

// seedLabel is the NoCloud drive's label. It is a variable so that the build can stamp it with
// -ldflags -X: the QEMU test of an upgrade that has to roll back builds a naru-init that cannot
// find its configuration drive. No other build changes it.
var seedLabel = "cidata"

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
	run           runFunc
	grow          growFunc

	seedMountPoint string
	resolvConf     string
	links          links

	dataMountPoint string
	// mvmdStorage is the storage mount point that mvmdState's disks and snapshots are linked into.
	mvmdStorage string
	mvmdState   string
	// storageMounts are the mount points of the storage disks, in the order they were mounted,
	// and unmount is syscall.Unmount when nil.
	mu            sync.Mutex
	storageMounts []string
	unmount       func(dir string) error

	// carrierWait bounds the wait for a link with a carrier; leaseWait, the first DHCPv4 exchange
	// (leaseWait when 0). dhcpOpen opens the DHCP transport of a link (the packet sockets when nil)
	// and dhcpClock is the clock of the exchange (the system clock when nil).
	carrierWait time.Duration
	leaseWait   time.Duration
	dhcpOpen    func(link) (dhcp4.Transport, error)
	dhcpClock   dhcp4.Clock
	leases      *leaseState

	time *timesync.Syncer
}

// NewHost returns the Host of the running machine, whose programs (mkfs) run through reaper. A disk
// or a network interface that has not appeared when it is first looked for is waited for up to
// diskWait.
func NewHost(diskWait time.Duration, reaper *Reaper) *Host {
	return &Host{
		sysClassBlock: "/sys/class/block",
		devDir:        "/dev",
		procSys:       "/proc/sys",
		diskWait:      diskWait,
		mount:         syscall.Mount,
		run:           reaper.Output,
		grow:          growFS,

		seedMountPoint: seedMountPoint,
		mvmdStorage:    wire.MvmdStorageMount,
		mvmdState:      wire.MvmdStateDir,
		resolvConf:     resolvConf,
		links:          netlinkLinks{},
		carrierWait:    carrierWait,
		time:           timesync.New(SystemClock{}, timesync.UDP{Now: time.Now}),
	}
}

// ReadSeed mounts the NoCloud drive read-only and returns its user-data, the
// Node's naru.yaml, and its network-config. When no disk carries the label it returns an error
// wrapping boot.ErrNoSeed, after waiting up to diskWait for one.
func (h *Host) ReadSeed() (userData, networkConfig []byte, err error) {
	dev, err := h.find(isoLabel, isSeedLabel)
	if errors.Is(err, errNoDisk) {
		return nil, nil, fmt.Errorf("NoCloud drive %q: %w: %w", seedLabel, err, boot.ErrNoSeed)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("NoCloud drive %q: %w", seedLabel, err)
	}
	if err := os.MkdirAll(h.seedMountPoint, 0o755); err != nil {
		return nil, nil, err
	}
	if err := h.mount(dev, h.seedMountPoint, "iso9660", syscall.MS_RDONLY|hardened, ""); err != nil {
		return nil, nil, fmt.Errorf("mount %s: %w", dev, err)
	}
	if userData, err = os.ReadFile(filepath.Join(h.seedMountPoint, "user-data")); err != nil {
		return nil, nil, err
	}
	// A drive without a network-config leaves the network to DHCPv4 (netcfg.Parse of nothing).
	if networkConfig, err = os.ReadFile(filepath.Join(h.seedMountPoint, netcfg.FileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	return userData, networkConfig, nil
}

// MountDataDisk finds the data disk, checks that it is ext4 with the verity
// feature, and mounts it. It mounts nothing from a disk that fails the check.
func (h *Host) MountDataDisk(d config.DataDisk) error {
	dev := d.Device
	if dev == "" {
		var err error
		if dev, err = h.find(ext4Label, func(got string) bool { return got == d.Label }); err != nil {
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

// WriteRegistries writes the files of spec.registries and spec.aws under init's directory.
func (h *Host) WriteRegistries(spec config.Spec) error {
	return registry.Writer{Dir: registry.Dir}.Write(spec)
}

// SetAsidePair sets the pair file of p aside when its certificate lacks the Organization p names.
func (h *Host) SetAsidePair(p config.KeptPair) (bool, error) { return keptpair.SetAside(p) }

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

// labelReader returns the label of the file system on a device, or an error if it holds none of its kind.
type labelReader func(io.ReaderAt) (string, error)

func ext4Label(r io.ReaderAt) (string, error) {
	sb, err := ext4.ReadSuperblock(r)
	return sb.Label, err
}

func isoLabel(r io.ReaderAt) (string, error) { return iso9660.ReadLabel(r) }

// isSeedLabel matches the NoCloud label in either case, as cloud-init does.
func isSeedLabel(got string) bool { return strings.EqualFold(got, seedLabel) }

// find returns the block device whose label, read by read, satisfies match,
// waiting up to diskWait for it to appear.
func (h *Host) find(read labelReader, match func(string) bool) (string, error) {
	return h.wait(func() (string, error) { return h.scan(read, match) })
}

// wait returns what look returns, trying again every diskPollInterval for up to diskWait while it
// returns errNoDisk.
func (h *Host) wait(look func() (string, error)) (string, error) {
	deadline := time.Now().Add(h.diskWait)
	for {
		dev, err := look()
		if err == nil || !errors.Is(err, errNoDisk) || time.Now().After(deadline) {
			return dev, err
		}
		time.Sleep(diskPollInterval)
	}
}

// scan reads the label of each device in /sys/class/block.
func (h *Host) scan(read labelReader, match func(string) bool) (string, error) {
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
		label, err := read(f)
		f.Close()
		if err == nil && match(label) {
			return dev, nil
		}
	}
	return "", errNoDisk
}
