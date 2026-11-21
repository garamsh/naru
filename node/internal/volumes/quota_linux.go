package volumes

import (
	"fmt"
	"os"
	"slices"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// ioctls FS_IOC_FSGETXATTR and FS_IOC_FSSETXATTR, and the flag that makes a directory's new files take its project.
	fsIocGetXattr    = 0x801c581f
	fsIocSetXattr    = 0x401c5820
	xflagProjInherit = 0x200

	// prjQuota is the quota type of projects.
	prjQuota = 2
	// qXSetQLim is QCMD(Q_XSETQLIM, PRJQUOTA).
	qXSetQLim = (0x5804 << 8) | prjQuota
	// fsDQuotVersion, fsProjQuota and fsDQBHard are the fs_disk_quota version, the project type
	// and the field mask bit of the hard block limit.
	fsDQuotVersion = 1
	fsProjQuota    = 2
	fsDQBHard      = 1 << 3
)

// fsxattr is struct fsxattr of the FS_IOC_FSGETXATTR ioctls.
type fsxattr struct {
	xflags, extsize, nextents, projid, cowextsize uint32
	pad                                           [8]byte
}

// diskQuota is struct fs_disk_quota of Q_XSETQLIM.
type diskQuota struct {
	version, flags                int8
	fieldmask                     uint16
	id                            uint32
	blkHard, blkSoft              uint64
	inoHard, inoSoft              uint64
	bcount, icount                uint64
	itimer, btimer                int32
	iwarns, bwarns                uint16
	itimerHi, btimerHi, rtbHi, p2 int8
	rtbHard, rtbSoft, rtbcount    uint64
	rtbtimer                      int32
	rtbwarns                      uint16
	p3                            int16
	p4                            [8]byte
}

// XFSQuota sets the project quotas of an XFS file system mounted with prjquota.
type XFSQuota struct{}

// Enabled returns an error unless the mount holding dir is XFS with prjquota (or pquota).
func (XFSQuota) Enabled(dir string) error {
	entries, err := readMountInfo()
	if err != nil {
		return err
	}
	return projectQuotaOf(entries, dir)
}

// projectQuotaOf checks the mount that holds dir.
func projectQuotaOf(entries []mountEntry, dir string) error {
	m, ok := holding(entries, dir)
	switch {
	case !ok:
		return fmt.Errorf("%s is on no mount", dir)
	case m.fsType != "xfs":
		return fmt.Errorf("%s is on %s, not xfs", dir, m.fsType)
	case !slices.Contains(m.superOptions, "prjquota") && !slices.Contains(m.superOptions, "pquota"):
		return fmt.Errorf("%s is mounted without prjquota", m.point)
	}
	return nil
}

// Set gives dir the project, makes its new files take it and sets the project's hard block limit.
func (XFSQuota) Set(dir string, project uint32, bytes int64) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	var attr fsxattr
	if err := ioctl(f.Fd(), fsIocGetXattr, unsafe.Pointer(&attr)); err != nil {
		return fmt.Errorf("get the attributes of %s: %w", dir, err)
	}
	attr.projid, attr.xflags = project, attr.xflags|xflagProjInherit
	if err := ioctl(f.Fd(), fsIocSetXattr, unsafe.Pointer(&attr)); err != nil {
		return fmt.Errorf("set the project of %s: %w", dir, err)
	}
	entries, err := readMountInfo()
	if err != nil {
		return err
	}
	m, ok := holding(entries, dir)
	if !ok {
		return fmt.Errorf("%s is on no mount", dir)
	}
	dev, err := unix.BytePtrFromString(m.source)
	if err != nil {
		return err
	}
	// The limit is in 512-byte blocks.
	dq := diskQuota{version: fsDQuotVersion, flags: fsProjQuota, fieldmask: fsDQBHard, id: project, blkHard: uint64(bytes+511) / 512}
	if _, _, errno := unix.Syscall6(unix.SYS_QUOTACTL, qXSetQLim, uintptr(unsafe.Pointer(dev)), uintptr(project), uintptr(unsafe.Pointer(&dq)), 0, 0); errno != 0 {
		return fmt.Errorf("limit project %d on %s: %w", project, m.source, errno)
	}
	return nil
}

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}
