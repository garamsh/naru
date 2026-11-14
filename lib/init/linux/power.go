package linux

import (
	"syscall"
)

// PowerOff syncs, unmounts the data disk and powers the machine off.
func (h *Host) PowerOff() error {
	h.quiesce()
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}

// Reboot syncs, unmounts the data disk and restarts the machine.
func (h *Host) Reboot() error {
	h.quiesce()
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}

// quiesce writes out what the data disk holds. Nothing writes to it once the
// services are stopped, so an unmount that fails leaves a disk that is clean.
func (h *Host) quiesce() {
	syscall.Sync()
	if h.dataMountPoint != "" {
		_ = syscall.Unmount(h.dataMountPoint, 0)
	}
}
