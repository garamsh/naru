package boot

import (
	"fmt"
	"log/slog"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/store"
)

// mountData mounts the data disk and then carries out the reset the host asked for before it
// rebooted, if one is waiting (ADR 0082). The request is read and removed first, so a boot that
// fails part of the way is not asked again.
//
//   - store discards the stored configuration, so the base runs as revision 0.
//   - data removes everything on the disk except the stored configuration.
//   - all unmounts the disk and blanks it, formats it as a blank disk is, and mounts it again, so
//     the Node starts as it did on its first boot.
//
// A reset that fails is a refusal: the Node must not run on a disk it was asked to clear and could
// not.
func mountData(host Host, configs Configs, disk config.DataDisk, log *slog.Logger) error {
	if err := host.MountDataDisk(disk); err != nil {
		return err
	}
	scope, found, err := configs.TakeReset()
	if !found && err == nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("the reset the host asked for cannot be read: %w", err)
	}
	log.Warn("resetting as the host asked", "scope", scope)
	switch scope {
	case store.ScopeStore:
		err = configs.Discard()
	case store.ScopeData:
		err = host.ClearDataDisk()
	case store.ScopeAll:
		err = host.WipeDataDisk(disk)
		if err == nil {
			err = host.FormatBlankDataDisk(disk, log)
		}
		if err == nil {
			err = host.MountDataDisk(disk)
		}
	}
	if err != nil {
		return fmt.Errorf("reset of scope %s: %w", scope, err)
	}
	log.Warn("reset done", "scope", scope)
	return nil
}
