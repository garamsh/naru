// Package boot is init's start-up sequence: read the configuration, check and
// mount the data disk, set the kernel up, and start the services.
package boot

import (
	"fmt"
	"log/slog"

	"github.com/garamsh/naru/init/internal/config"
)

// Host is the machine as the sequence needs it.
type Host interface {
	// ReadConfig returns the bytes of the configuration file.
	ReadConfig() ([]byte, error)
	// MountDataDisk mounts the data disk, or refuses it.
	MountDataDisk(config.DataDisk) error
	// EnableForwarding turns forwarding on for the families.
	EnableForwarding([]config.Family) error
	// SealRoot remounts the root file system read-only. Every mount point
	// the machine needs exists by then.
	SealRoot() error
}

// Services runs the services.
type Services interface {
	Prepare() error
	Start(config.Service) error
}

// Run starts the services of the configuration the host holds. It starts
// none, and returns the reason, when the configuration is invalid, the data
// disk is refused or the kernel cannot be set up: a refusal is the same
// whichever service it would have kept from starting.
func Run(host Host, services Services, log *slog.Logger) error {
	raw, err := host.ReadConfig()
	if err != nil {
		return fmt.Errorf("read configuration: %w", err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	log.Info("configuration accepted", "node", cfg.Metadata.Name, "services", len(cfg.Spec.Services))

	if err := host.MountDataDisk(cfg.Spec.DataDisk); err != nil {
		return err
	}
	if err := host.EnableForwarding(cfg.Spec.Network.Forwarding); err != nil {
		return err
	}
	if err := services.Prepare(); err != nil {
		return fmt.Errorf("prepare cgroups: %w", err)
	}
	if err := host.SealRoot(); err != nil {
		return fmt.Errorf("seal the root: %w", err)
	}
	log.Info("root remounted read-only")
	for _, svc := range cfg.Spec.Services {
		if svc.ControlPlane && !cfg.Spec.ControlPlane {
			log.Info("skipping a control-plane service on a Node that is not one", "service", svc.Name)
			continue
		}
		if err := services.Start(svc); err != nil {
			return fmt.Errorf("start %s: %w", svc.Name, err)
		}
	}
	return nil
}
