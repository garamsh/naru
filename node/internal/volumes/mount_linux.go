package volumes

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// BindMounter binds with mount(2).
type BindMounter struct{}

// Bind mounts source at target as a bind mount that allows neither devices nor setuid programs on a
// directory; a device node is bound as it is, since the Pod is to use it.
func (BindMounter) Bind(source, target string, readOnly bool) error {
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		return err
	}
	if !readOnly {
		return nil
	}
	return unix.Mount("", target, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, "")
}

func (BindMounter) Unmount(target string) error { return unix.Unmount(target, 0) }

func (BindMounter) Mounted(target string) (bool, error) {
	entries, err := readMountInfo()
	if err != nil {
		return false, err
	}
	return isMountPoint(entries, filepath.Clean(target)), nil
}
