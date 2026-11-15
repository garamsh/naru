package linux

import (
	"log/slog"
	"syscall"
)

// PowerOff syncs and powers the machine off. Settle runs first.
func (h *Host) PowerOff() error {
	syscall.Sync()
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}

// Reboot syncs and restarts the machine. Settle runs first.
func (h *Host) Reboot() error {
	syscall.Sync()
	return syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}

// Settle unmounts what the machine holds, once the services are stopped and nothing writes any
// more: the system extensions at extensions, then the storage disks, then the data disk, which
// the others may sit on. It goes on past a failure, so one busy mount does not leave the rest
// mounted, and logs each.
func (h *Host) Settle(log *slog.Logger, extensions []string) {
	syscall.Sync()
	unmount := h.unmount
	if unmount == nil {
		unmount = func(dir string) error { return syscall.Unmount(dir, 0) }
	}
	h.mu.Lock()
	storage := append([]string(nil), h.storageMounts...)
	h.mu.Unlock()
	stages := []struct {
		kind string
		dirs []string
	}{{"extension", extensions}, {"storage disk", reverse(storage)}}
	if h.dataMountPoint != "" {
		stages = append(stages, struct {
			kind string
			dirs []string
		}{"data disk", []string{h.dataMountPoint}})
	}
	for _, st := range stages {
		for _, dir := range st.dirs {
			if err := unmount(dir); err != nil {
				log.Error("unmount at shutdown failed", "kind", st.kind, "mount", dir, "err", err)
				continue
			}
			log.Info("unmounted at shutdown", "kind", st.kind, "mount", dir)
		}
	}
}

// reverse returns dirs last to first, the order mounts are undone in.
func reverse(dirs []string) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[len(dirs)-1-i] = d
	}
	return out
}
