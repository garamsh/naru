package linux

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

type pseudoMount struct {
	source, target, fstype string
	flags                  uintptr
	data                   string
}

const (
	nosuidNodev = syscall.MS_NOSUID | syscall.MS_NODEV
	hardened    = nosuidNodev | syscall.MS_NOEXEC
)

// pseudoMounts are in the order they are mounted: a target inside another is listed after it.
var pseudoMounts = []pseudoMount{
	{"proc", "/proc", "proc", hardened, ""},
	{"sysfs", "/sys", "sysfs", hardened, ""},
	{"devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755"},
	{"devpts", "/dev/pts", "devpts", syscall.MS_NOSUID | syscall.MS_NOEXEC, "mode=0620,ptmxmode=0666"},
	{"tmpfs", "/run", "tmpfs", nosuidNodev, "mode=0755"},
	{"tmpfs", "/tmp", "tmpfs", nosuidNodev, "mode=1777"},
	{"cgroup2", "/sys/fs/cgroup", "cgroup2", hardened, ""},
}

// MountPseudoFilesystems mounts /proc, /sys, /dev, /dev/pts, /run, /tmp and
// /sys/fs/cgroup, and makes Ctrl-Alt-Del deliver SIGINT to init instead of
// rebooting the machine at once. A mount that reports EBUSY is taken as done.
func MountPseudoFilesystems() error {
	for _, m := range pseudoMounts {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(m.source, m.target, m.fstype, m.flags, m.data); err != nil && !errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("mount %s on %s: %w", m.fstype, m.target, err)
		}
	}
	return os.WriteFile("/proc/sys/kernel/ctrl-alt-del", []byte("0"), 0o644)
}
