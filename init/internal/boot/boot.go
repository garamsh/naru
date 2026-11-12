// Package boot is init's start-up sequence: read the configuration and the
// network from the NoCloud drive, bring the network up, check and mount the
// data disk, set the kernel up, and start the services.
package boot

import (
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/netcfg"
)

// Host is the machine as the sequence needs it.
type Host interface {
	// ReadSeed returns the bytes of the NoCloud drive's user-data, which is
	// naru.yaml, and of its network-config.
	ReadSeed() (userData, networkConfig []byte, err error)
	// ApplyNetwork brings the network up and writes the resolvers.
	ApplyNetwork(netcfg.Config) error
	// MountDataDisk mounts the data disk, or refuses it.
	MountDataDisk(config.DataDisk) error
	// EnableForwarding turns forwarding on for the families.
	EnableForwarding([]config.Family) error
	// WriteFiles writes the files the configuration carries.
	WriteFiles([]config.File) error
	// SealRoot remounts the root file system read-only. Every mount point
	// the machine needs exists by then.
	SealRoot() error
}

// Issuer issues the certificates of spec.pki.
type Issuer interface {
	// Issue writes the authorities' certificates and issues the certificates,
	// with addrs as the Node's addresses.
	Issue(spec config.PKI, addrs []netip.Addr) error
}

// Services runs the services.
type Services interface {
	Prepare() error
	Start(config.Service) error
}

// Run starts the services of the configuration the host holds. It starts
// none, and returns the reason, when the configuration or the network-config
// is invalid, the network cannot be brought up, the data disk is refused, the
// kernel cannot be set up or a certificate cannot be issued: a refusal is the same whichever service it would
// have kept from starting.
func Run(host Host, issuer Issuer, services Services, log *slog.Logger) error {
	userData, networkConfig, err := host.ReadSeed()
	if err != nil {
		return fmt.Errorf("read the NoCloud drive: %w", err)
	}
	cfg, err := config.Parse(userData)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	network, err := netcfg.Parse(networkConfig)
	if err != nil {
		return fmt.Errorf("invalid network configuration: %w", err)
	}
	log.Info("configuration accepted", "node", cfg.Metadata.Name, "services", len(cfg.Spec.Services))

	if err := host.ApplyNetwork(network); err != nil {
		return fmt.Errorf("apply network configuration: %w", err)
	}
	log.Info("network configured", "interfaces", len(network.Interfaces), "nameservers", len(network.Nameservers))

	if err := host.MountDataDisk(cfg.Spec.DataDisk); err != nil {
		return err
	}
	if err := host.EnableForwarding(cfg.Spec.Network.Forwarding); err != nil {
		return err
	}
	if err := host.WriteFiles(cfg.Spec.Files); err != nil {
		return err
	}
	if pki := cfg.Spec.PKI; len(pki.Authorities) > 0 || len(pki.Certificates) > 0 {
		var addrs []netip.Addr
		for _, iface := range network.Interfaces {
			for _, p := range iface.Addresses {
				addrs = append(addrs, p.Addr())
			}
		}
		if err := issuer.Issue(pki, addrs); err != nil {
			return fmt.Errorf("issue certificates: %w", err)
		}
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
