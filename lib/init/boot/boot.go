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

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/netcfg"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/timesync"
)

// Host is the machine as the sequence needs it.
type Host interface {
	// ReadSeed returns the bytes of the NoCloud drive's user-data, which is
	// naru.yaml, and of its network-config.
	ReadSeed() (userData, networkConfig []byte, err error)
	// ApplyNetwork brings the network up and writes the resolvers.
	ApplyNetwork(netcfg.Config) error
	// FormatBlankDataDisk formats the blank disk spec.dataDisk.formatBlank names, and logs it.
	FormatBlankDataDisk(config.DataDisk, *slog.Logger) error
	// MountDataDisk mounts the data disk, or refuses it.
	MountDataDisk(config.DataDisk) error
	// EnableForwarding turns forwarding on for the families.
	EnableForwarding([]config.Family) error
	// AddRoute installs a static route of spec.network.routes.
	AddRoute(config.Route) error
	// WriteFiles writes the files the configuration carries.
	WriteFiles([]config.File) error
	// WriteRegistries writes the files of spec.registries and spec.aws.
	WriteRegistries(config.Spec) error
	// SetAsidePair sets the pair file of p aside when its certificate lacks the Organization p
	// names, and reports whether it did.
	SetAsidePair(config.KeptPair) (bool, error)
	// MountExtensions mounts the system extensions of spec.extensions read-only under their
	// directories, or refuses: one that is missing, built for another version or fails its digest.
	MountExtensions([]config.Extension) error
	// PrepareStorage finds a storage disk, formats it when it is blank, mounts it and grows its
	// file system to the device, logging what it does.
	PrepareStorage(config.Storage, *slog.Logger) error
	// SyncTime synchronises the clock with the servers, each a host:port, and returns the
	// result, or an error when none answers within the bound.
	SyncTime(servers []string) (timesync.Result, error)
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

// API serves the Node API.
type API interface {
	// Start serves the API for the chosen configuration, or logs that it is off.
	Start(store.Stored) error
}

// Run starts the services of the effective configuration: the stored one,
// when it was derived from the user-data on the NoCloud drive and validates,
// and the user-data's otherwise. The steps before the data disk mounts use the
// user-data. It starts none, and returns the reason, when the configuration or
// the network-config is invalid, the network cannot be brought up, the data
// disk is refused, a system extension cannot be mounted, the Node API cannot listen, the kernel cannot be set up or
// a certificate cannot be issued: a refusal is the same whichever service it would have kept from
// starting. A clock that no time server could set is not one: it is logged, and the boot goes on.
// Nor is a route the kernel refuses, nor a storage disk that is missing or cannot be prepared: each is logged too.
// On success it returns the effective configuration and its revision.
func Run(host Host, issuer Issuer, services Services, configs Configs, api API, log *slog.Logger) (store.Stored, error) {
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

	if base.Spec.DataDisk.FormatBlank {
		if err := host.FormatBlankDataDisk(base.Spec.DataDisk, log); err != nil {
			return store.Stored{}, err
		}
	}
	if err := host.MountDataDisk(base.Spec.DataDisk); err != nil {
		return store.Stored{}, err
	}
	effective := choose(configs, store.Digest(userData), base, log)
	cfg := effective.Config
	// The API starts once the configuration is chosen, before anything else can refuse, so a
	// Node that refuses after this still answers reads.
	if err := api.Start(effective); err != nil {
		return store.Stored{}, fmt.Errorf("start the Node API: %w", err)
	}
	if err := host.EnableForwarding(cfg.Spec.Network.Forwarding); err != nil {
		return store.Stored{}, err
	}
	addRoutes(host, cfg.Spec.Network.Routes, log)
	if err := host.WriteFiles(cfg.Spec.Files); err != nil {
		return store.Stored{}, err
	}
	if err := host.WriteRegistries(cfg.Spec); err != nil {
		return store.Stored{}, fmt.Errorf("write the registry files: %w", err)
	}
	if len(cfg.Spec.Extensions) > 0 {
		if err := host.MountExtensions(cfg.Spec.Extensions); err != nil {
			return store.Stored{}, fmt.Errorf("mount the system extensions: %w", err)
		}
	}
	prepareStorage(host, cfg.Spec.Storage, log)
	syncTime(host, cfg.Spec.Time, log)
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
		setAsidePair(host, svc, log)
		if err := services.Start(svc); err != nil {
			return store.Stored{}, fmt.Errorf("start %s: %w", svc.Name, err)
		}
	}
	return effective, nil
}

// setAsidePair sets aside the pair svc kept when its certificate lacks the Organization svc needs
// (config.KeptPair), so svc asks for a new one. It never refuses the boot: a pair that cannot be
// set aside is logged at error level and svc starts, and reports what it cannot use.
func setAsidePair(host Host, svc config.Service, log *slog.Logger) {
	if svc.KeptPair == nil {
		return
	}
	set, err := host.SetAsidePair(*svc.KeptPair)
	switch {
	case err != nil:
		log.Error("kept pair not set aside; the boot goes on", "service", svc.Name, "file", svc.KeptPair.File, "err", err)
	case set:
		log.Warn("kept pair set aside: its certificate has no required Organization", "service", svc.Name, "file", svc.KeptPair.File, "organization", svc.KeptPair.Organization)
	}
}

// addRoutes installs spec.network.routes. It never refuses the boot: a route the kernel refuses is
// logged at error level and the rest are installed, so a stored route that no longer fits the
// network cannot take the Node, and its Node API, down.
func addRoutes(host Host, routes []config.Route, log *slog.Logger) {
	for _, r := range routes {
		if err := host.AddRoute(r); err != nil {
			log.Error("route not installed; the boot goes on", "to", r.To, "via", r.Via, "err", err)
			continue
		}
		log.Info("route installed", "to", r.To, "via", r.Via)
	}
}

// prepareStorage prepares each disk of spec.storage. It never refuses the boot: a disk that is
// missing, holds something else or cannot be mounted is logged at error level, so a Node without
// its extra disk boots as before and keeps its Node API.
func prepareStorage(host Host, storage []config.Storage, log *slog.Logger) {
	for _, s := range storage {
		if err := host.PrepareStorage(s, log); err != nil {
			log.Error("storage disk not prepared; the boot goes on", "name", s.Name, "err", err)
		}
	}
}

// syncTime sets the clock from spec.time once, before certificates are issued. It never refuses
// the boot: a clock that no server could set is logged and the boot goes on, and the Node keeps
// trying (timesync.Syncer.Run).
func syncTime(host Host, spec *config.Time, log *slog.Logger) {
	if spec == nil {
		log.Info("time is not synchronised: the configuration has no spec.time")
		return
	}
	res, err := host.SyncTime(spec.Addrs())
	if err != nil {
		log.Warn("time not synchronised; the boot goes on and init keeps trying", "err", err)
		return
	}
	log.Info("time synchronised", "sync", res)
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
