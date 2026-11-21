package linux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/xfs"
)

const (
	mkfsXFS = "/bin/mkfs.xfs"
	// blankProbe is how much of each end of a disk must be zeros for it to count as blank.
	blankProbe = 1 << 20
	// xfsIocFSGrowFSData is XFS_IOC_FSGROWFSDATA, _IOW('X', 110, struct xfs_growfs_data).
	xfsIocFSGrowFSData = 0x4010586e
)

// runFunc runs a program and returns its combined output.
type runFunc func(name string, args ...string) ([]byte, error)

// growFunc grows the XFS file system mounted at dir to blocks data blocks.
type growFunc func(dir string, blocks uint64, imaxPct uint8) error

// growFS is XFS_IOC_FSGROWFSDATA on the mount point, the call xfs_growfs makes.
func growFS(dir string, blocks uint64, imaxPct uint8) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	// struct xfs_growfs_data { __u64 newblocks; __u32 imaxpct; }, padded to 16 bytes.
	arg := struct {
		newBlocks uint64
		imaxPct   uint32
	}{blocks, uint32(imaxPct)}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), xfsIocFSGrowFSData, uintptr(unsafe.Pointer(&arg))); errno != 0 {
		return errno
	}
	return nil
}

// PrepareStorage finds the disk of s, formats it as XFS when it is blank, mounts it hardened and
// grows the file system to the size of the device. It never formats a disk that holds anything,
// leaves a disk that holds something other than XFS alone, so the data disk is never taken, and
// mounts nothing over a directory that already holds files. It logs what it did.
func (h *Host) PrepareStorage(s config.Storage, log *slog.Logger) error {
	dev, err := h.findStorage(s)
	if err != nil {
		return fmt.Errorf("storage %q: %w", s.Name, err)
	}
	mnt := s.Mount()
	if entries, err := os.ReadDir(mnt); err == nil && len(entries) > 0 {
		return fmt.Errorf("storage %q: %s already holds %d entries, which a mount would hide; nothing is mounted over them", s.Name, mnt, len(entries))
	}

	f, err := os.Open(dev)
	if err != nil {
		return fmt.Errorf("storage %q: %w", s.Name, err)
	}
	defer f.Close()
	sb, err := xfs.ReadSuperblock(f)
	switch {
	case err == nil:
	case errors.Is(err, xfs.ErrNotXFS):
		blank, berr := isBlankDisk(f)
		if berr != nil {
			return fmt.Errorf("storage %q: probe %s: %w", s.Name, dev, berr)
		}
		if !blank {
			return fmt.Errorf("storage %q: %s holds a file system or data that is not XFS; it is left alone", s.Name, dev)
		}
		if out, err := h.run(mkfsXFS, "-m", "reflink=1,crc=1", "-L", s.Name, dev); err != nil {
			return fmt.Errorf("storage %q: mkfs.xfs %s: %w: %s", s.Name, dev, err, strings.TrimSpace(string(out)))
		}
		log.Info("storage disk formatted as XFS", "name", s.Name, "device", dev)
		if sb, err = xfs.ReadSuperblock(f); err != nil {
			return fmt.Errorf("storage %q: read %s after mkfs.xfs: %w", s.Name, dev, err)
		}
	default:
		return fmt.Errorf("storage %q: %w", s.Name, err)
	}
	// An entry given by device or by serial finds its XFS by the label init gave it, so an XFS
	// there is another disk's.
	if s.Label == "" && sb.Label != s.Name {
		return fmt.Errorf("storage %q: %s holds an XFS file system labelled %q, not %q; it is left alone", s.Name, dev, sb.Label, s.Name)
	}

	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return fmt.Errorf("storage %q: %w", s.Name, err)
	}
	if err := h.mount(dev, mnt, "xfs", hardened, "prjquota"); err != nil {
		return fmt.Errorf("storage %q: mount %s on %s: %w", s.Name, dev, mnt, err)
	}
	h.mu.Lock()
	h.storageMounts = append(h.storageMounts, mnt)
	h.mu.Unlock()
	// Only the daemon lists the storage mount that mvmd keeps its disks in.
	if err := os.Chmod(mnt, 0o700); err != nil {
		return fmt.Errorf("storage %q: %w", s.Name, err)
	}
	log.Info("storage disk mounted", "name", s.Name, "device", dev, "mount", mnt, "blocks", sb.DataBlocks, "blockSize", sb.BlockSize)
	if mnt == h.mvmdStorage {
		if err := h.linkMvmdStorage(mnt, log); err != nil {
			return fmt.Errorf("storage %q: %w", s.Name, err)
		}
		log.Info("mvmd's disks, snapshots and disk snapshots linked into storage", "mount", mnt)
	}

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("storage %q: size of %s: %w", s.Name, dev, err)
	}
	if blocks, grow := sb.NewBlocks(size); grow {
		if err := h.grow(mnt, blocks, sb.ImaxPct); err != nil {
			return fmt.Errorf("storage %q: grow %s: %w", s.Name, mnt, err)
		}
		log.Info("storage file system grown", "name", s.Name, "from", sb.DataBlocks, "to", blocks, "blockSize", sb.BlockSize)
	}
	return nil
}

// mvmdLinked are the directories of mvmd's state directory that live on the storage mount.
var mvmdLinked = []string{"disks", "snapshots", "disk-snapshots"}

// linkMvmdStorage makes each of mvmdLinked a directory of the storage mount at mnt, which only the
// daemon may list, and a link to it in mvmd's state directory. An empty directory there, as the
// mount point of an earlier release left, is replaced; a link that is right stays; a directory
// that holds entries has them moved into the storage target first (moveEntry), since a link
// would hide them, and an entry whose name is taken there is an error.
func (h *Host) linkMvmdStorage(mnt string, log *slog.Logger) error {
	if err := os.MkdirAll(h.mvmdState, 0o700); err != nil {
		return err
	}
	for _, name := range mvmdLinked {
		target, link := filepath.Join(mnt, name), filepath.Join(h.mvmdState, name)
		if err := os.MkdirAll(target, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(target, 0o700); err != nil {
			return err
		}
		if err := linkTo(link, target, log); err != nil {
			return err
		}
	}
	return nil
}

// linkTo makes link a symbolic link to target. A directory at link has its entries moved into
// target first.
func linkTo(link, target string, log *slog.Logger) error {
	info, err := os.Lstat(link)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case info.Mode()&fs.ModeSymlink != 0:
		if got, err := os.Readlink(link); err != nil || got != target {
			return fmt.Errorf("%s links to %q, not to %s; mvmd will refuse to start with its disks, snapshots and disk snapshots on two file systems", link, got, target)
		}
		return nil
	case info.IsDir():
		if err := removeTombstones(link); err != nil {
			return err
		}
		entries, err := os.ReadDir(link)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := moveEntry(filepath.Join(link, e.Name()), filepath.Join(target, e.Name()), log); err != nil {
				return err
			}
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%s is not a directory; mvmd will refuse to start", link)
	}
	return os.Symlink(target, link)
}

// isBlankDisk is the one rule for a blank disk, for the data disk and for a storage disk: f holds at
// least one byte, and its first and its last blankProbe bytes (all of it when it is smaller) are
// zeros, so it has no partition table, no file system signature and no backup GPT. Data only
// between the two probes is not seen.
func isBlankDisk(f *os.File) (bool, error) {
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil || size == 0 {
		return false, err
	}
	if ok, err := zeros(f, 0); err != nil || !ok {
		return false, err
	}
	if size <= blankProbe {
		return true, nil
	}
	return zeros(f, size-blankProbe)
}

// zeros reports whether the blankProbe bytes of r at off, or those that exist, are all zero.
func zeros(r io.ReaderAt, off int64) (bool, error) {
	buf := make([]byte, blankProbe)
	n, err := r.ReadAt(buf, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(bytes.Trim(buf[:n], "\x00")) == 0, nil
}

// findStorage returns the device of s, waiting up to diskWait for it to appear. A label matches a
// GPT partition name or the label of an XFS file system. An entry given by device or by serial is
// found first as the XFS that init labelled with its name, wherever the kernel put it, and only
// when there is none is the device taken, or the one disk of that serial, for a blank disk.
func (h *Host) findStorage(s config.Storage) (string, error) {
	if s.Label != "" {
		return h.wait(func() (string, error) { return h.scanStorage(s.Label, true) })
	}
	return h.wait(func() (string, error) {
		dev, err := h.scanStorage(s.Name, false)
		if err == nil || !errors.Is(err, errNoDisk) {
			return dev, err
		}
		if s.Serial != "" {
			return h.scanSerial(s.Serial)
		}
		if _, err := os.Stat(s.Device); err != nil {
			return "", fmt.Errorf("%s: %w", s.Device, errNoDisk)
		}
		return s.Device, nil
	})
}

// scanSerial returns the one whole disk in /sys/class/block whose serial number is serial. No such
// disk is errNoDisk, so that the disk is waited for; two are an error, since init cannot tell which
// is meant and formats neither.
func (h *Host) scanSerial(serial string) (string, error) {
	entries, err := os.ReadDir(h.sysClassBlock)
	if err != nil {
		return "", err
	}
	var found []string
	for _, e := range entries {
		if diskHasSerial(filepath.Join(h.sysClassBlock, e.Name()), serial) {
			found = append(found, filepath.Join(h.devDir, e.Name()))
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("serial %q: %w", serial, errNoDisk)
	case 1:
		return found[0], nil
	}
	return "", fmt.Errorf("serial %q is the serial of %d disks (%s); none is used", serial, len(found), strings.Join(found, ", "))
}

// diskHasSerial reports whether the block device at sys (a directory of /sys/class/block) has the
// serial number serial, as a virtio-blk disk gives it in serial, or a SCSI disk in the payload of
// its VPD page 0x80 or as its wwid.
func diskHasSerial(sys, serial string) bool {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(sys, name))
		if err != nil {
			return ""
		}
		return string(b)
	}
	clean := func(s string) string { return strings.Trim(s, " \t\r\n\x00") }
	if clean(read("serial")) == serial || clean(read("device/wwid")) == serial {
		return true
	}
	// A VPD page has a four-byte header before the serial number.
	page := read("device/vpd_pg80")
	return len(page) > 4 && clean(page[4:]) == serial
}

// scanStorage returns the first device in /sys/class/block that holds an XFS file system labelled
// label or, when partitions is set, is a partition named label.
func (h *Host) scanStorage(label string, partitions bool) (string, error) {
	entries, err := os.ReadDir(h.sysClassBlock)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		dev := filepath.Join(h.devDir, e.Name())
		if partitions && partName(filepath.Join(h.sysClassBlock, e.Name(), "uevent")) == label {
			return dev, nil
		}
		f, err := os.Open(dev)
		if err != nil {
			continue
		}
		sb, err := xfs.ReadSuperblock(f)
		f.Close()
		if err == nil && sb.Label == label {
			return dev, nil
		}
	}
	return "", fmt.Errorf("%q: %w", label, errNoDisk)
}

// partName returns the PARTNAME of a device's uevent file, or "" when it has none.
func partName(uevent string) string {
	data, err := os.ReadFile(uevent)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if name, ok := strings.CutPrefix(line, "PARTNAME="); ok {
			return name
		}
	}
	return ""
}
