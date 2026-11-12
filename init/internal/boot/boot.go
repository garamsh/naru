// Package boot is init's start-up sequence: read the configuration and the
// network from the NoCloud drive, bring the network up, check and mount the
// data disk, choose the effective configuration, set the kernel up, and start
// the services.
package boot

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/netcfg"
	"github.com/garamsh/naru/init/internal/store"
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

// Configs is the store of the Node's configuration revisions, on the data disk.
type Configs interface {
	// Load returns the stored configuration, store.ErrNotFound when there is
	// none, and another error when it cannot be read or does not validate.
	Load() (store.Stored, error)
	// Save stores st.
	Save(store.Stored) error
}

// Services runs the services.
type Services interface {
	Prepare() error
	Start(config.Service) error
}

// Run starts the services of the effective configuration: the stored one,
// when it was derived from the user-data on the NoCloud drive and validates,
// and the user-data's otherwise. The steps before the data disk mounts use the
// user-data. It starts none, and returns the reason, when the configuration or
// the network-config is invalid, the network cannot be brought up, the data
// disk is refused, the kernel cannot be set up or a certificate cannot be
// issued: a refusal is the same whichever service it would have kept from
// starting. On success it returns the effective configuration and its
// revision.
func Run(host Host, issuer Issuer, services Services, configs Configs, log *slog.Logger) (store.Stored, error) {
	userData, networkConfig, err := host.ReadSeed()
	if err != nil {
		return store.Stored{}, fmt.Errorf("read the NoCloud drive: %w", err)
	}
	base, err := config.Parse(userData)
	if err != nil {
		return store.Stored{}, fmt.Errorf("invalid configuration: %w", err)
	}
	network, err := netcfg.Parse(networkConfig)
	if err != nil {
		return store.Stored{}, fmt.Errorf("invalid network configuration: %w", err)
	}
	log.Info("configuration accepted", "node", base.Metadata.Name, "services", len(base.Spec.Services))

	if err := host.ApplyNetwork(network); err != nil {
		return store.Stored{}, fmt.Errorf("apply network configuration: %w", err)
	}
	log.Info("network configured", "interfaces", len(network.Interfaces), "nameservers", len(network.Nameservers))

	if err := host.MountDataDisk(base.Spec.DataDisk); err != nil {
		return store.Stored{}, err
	}
	effective := choose(configs, store.Digest(userData), base, log)
	cfg := effective.Config
	if err := host.EnableForwarding(cfg.Spec.Network.Forwarding); err != nil {
		return store.Stored{}, err
	}
	if err := host.WriteFiles(cfg.Spec.Files); err != nil {
		return store.Stored{}, err
	}
	if pki := cfg.Spec.PKI; len(pki.Authorities) > 0 || len(pki.Certificates) > 0 {
		var addrs []netip.Addr
		for _, iface := range network.Interfaces {
			for _, p := range iface.Addresses {
				addrs = append(addrs, p.Addr())
			}
		}
		if err := issuer.Issue(pki, addrs); err != nil {
			return store.Stored{}, fmt.Errorf("issue certificates: %w", err)
		}
	}
	if err := services.Prepare(); err != nil {
		return store.Stored{}, fmt.Errorf("prepare cgroups: %w", err)
	}
	if err := host.SealRoot(); err != nil {
		return store.Stored{}, fmt.Errorf("seal the root: %w", err)
	}
	log.Info("root remounted read-only")
	for _, svc := range cfg.Spec.Services {
		if !cfg.Spec.Runs(svc) {
			log.Info("skipping a control-plane service on a Node that is not one", "service", svc.Name)
			continue
		}
		if err := services.Start(svc); err != nil {
			return store.Stored{}, fmt.Errorf("start %s: %w", svc.Name, err)
		}
	}
	return effective, nil
}

// choose returns the effective configuration: the stored one when its base is
// the user-data the Node holds now and it validates, and base, at revision 0,
// otherwise. A stored configuration that is not used is discarded and
// replaced by the base, with the reason logged. A Node with no store runs its base.
func choose(configs Configs, digest string, base config.NodeConfig, log *slog.Logger) store.Stored {
	fresh := store.Stored{Revision: 0, BaseSha256: digest, Config: base}
	stored, err := configs.Load()
	var reason string
	switch {
	case errors.Is(err, store.ErrNotFound):
		log.Info("no stored configuration; running the base", "revision", 0)
		return fresh
	case err != nil:
		reason = "the stored configuration is not valid: " + err.Error()
	case stored.BaseSha256 != digest:
		reason = "the base changed since the configuration was stored"
	default:
		log.Info("running the stored configuration", "revision", stored.Revision)
		return stored
	}
	log.Warn("stored configuration discarded; running the base as revision 0", "reason", reason)
	if err := configs.Save(fresh); err != nil {
		log.Error("store the base as revision 0", "err", err)
	}
	return fresh
}
