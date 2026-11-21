package volumes

import (
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// BindMounter binds with mount(2).
type BindMounter struct{}

// Bind mounts source at target. A bind starts with the flags of the mount it copies, but a remount
// sets the flags it names and clears the rest, so the remount names the source mount's nodev, nosuid
// and noexec again, and adds read-only when asked. A directory is bound nodev and nosuid whatever its
// source says; a device node is bound as it is, since the Pod is to use it.
func (BindMounter) Bind(source, target string, readOnly bool) error {
	entries, err := readMountInfo()
	if err != nil {
		return err
	}
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	flags := remountFlags(entries, source, isDir(source), readOnly)
	if flags == 0 {
		return nil
	}
	return unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|flags, "")
}

func (BindMounter) Unmount(target string) error { return unix.Unmount(target, 0) }

func (BindMounter) Mounted(target string) (bool, error) {
	entries, err := readMountInfo()
	if err != nil {
		return false, err
	}
	return isMountPoint(entries, filepath.Clean(target)), nil
}

// remountFlags are the flags a bind of source is remounted with: the ones its mount has among nodev,
// nosuid and noexec, nodev and nosuid for a directory, and read-only when asked.
func remountFlags(entries []mountEntry, source string, dir, readOnly bool) uintptr {
	var flags uintptr
	if m, ok := holding(entries, source); ok {
		for opt, flag := range map[string]uintptr{"nodev": unix.MS_NODEV, "nosuid": unix.MS_NOSUID, "noexec": unix.MS_NOEXEC} {
			if slices.Contains(m.options, opt) {
				flags |= flag
			}
		}
	}
	if dir {
		flags |= unix.MS_NODEV | unix.MS_NOSUID
	}
	if readOnly {
		flags |= unix.MS_RDONLY
	}
	return flags
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
