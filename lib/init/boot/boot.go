// Package boot is init's start-up sequence: read the configuration and the
// network from the NoCloud drive, bring the network up, check and mount the
// data disk, choose the effective configuration, set the kernel up, and start
// the services.
package boot

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/netcfg"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/timesync"
	"github.com/garamsh/naru/lib/wire"
)

// ErrNoSeed is what Host.ReadSeed wraps when no NoCloud drive is attached, which is not a
// refusal: the Node then runs the configuration stored on its data disk.
var ErrNoSeed = errors.New("no NoCloud drive is attached")

// Host is the machine as the sequence needs it.
type Host interface {
	// ReadSeed returns the bytes of the NoCloud drive's user-data, which is
	// naru.yaml, and of its network-config. A drive with no network-config gives none. When no
	// drive is attached it returns an error wrapping ErrNoSeed.
	ReadSeed() (userData, networkConfig []byte, err error)
	// ApplyNetwork brings the network up and writes the resolvers, and returns what the Node was
	// given: its addresses, its resolvers and the time servers a DHCPv4 lease named.
	ApplyNetwork(netcfg.Config, *slog.Logger) (netcfg.Applied, error)
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
	// ClearDataDisk removes everything on the mounted data disk except the stored configuration:
	// the reset of scope data (ADR 0082).
	ClearDataDisk() error
	// WipeDataDisk unmounts the data disk and blanks it, so that the format of a blank disk finds it
	// blank: the reset of scope all (ADR 0082).
	WipeDataDisk(config.DataDisk) error
}

// Configs is the store of the Node's configuration revisions, on the data disk.
type Configs interface {
	// Load returns the stored configuration, store.ErrNotFound when there is
	// none, and another error when it cannot be read or does not validate.
	Load() (store.Stored, error)
	// Save stores st.
	Save(store.Stored) error
	// LoadNetwork returns the network-config stored beside the configuration, and
	// store.ErrNotFound when none was stored.
	LoadNetwork() ([]byte, error)
	// SaveNetwork stores the network-config of the seed the Node booted from.
	SaveNetwork([]byte) error
	// Discard removes the stored configuration, which is the reset of scope store (ADR 0082).
	Discard() error
	// TakeReset reads the reset naru-hostd asked for and removes the request. It returns false when
	// there was none.
	TakeReset() (scope string, found bool, err error)
	// LoadAnchor returns the maintenance anchor stored beside the configuration, and
	// store.ErrNotFound when none was stored. SaveAnchor stores it.
	LoadAnchor() ([]byte, error)
	SaveAnchor([]byte) error
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
// disk is refused, a system extension cannot be mounted, the Node API cannot listen or the kernel
// cannot be set up: a refusal is the same whichever service it would have kept from
// starting. A clock that no time server could set is not one: it is logged, and the boot goes on.
// Nor is a route the kernel refuses, nor a storage disk that is missing or cannot be prepared: each is logged too.
// On success it returns the effective configuration and its revision.
func Run(host Host, services Services, configs Configs, api API, log *slog.Logger) (store.Stored, error) {
	h := &watchedHost{Host: host}
	svc := &watchedServices{Services: services}
	stored, err := run(h, svc, configs, api, log)
	if err != nil {
		maintain(h, svc, configs, err, log)
	}
	return stored, err
}

// run is the sequence itself; Run enters maintenance mode when it refuses.
func run(host Host, services Services, configs Configs, api API, log *slog.Logger) (store.Stored, error) {
	var (
		effective store.Stored
		applied   netcfg.Applied
	)
	userData, networkConfig, err := host.ReadSeed()
	switch {
	case errors.Is(err, ErrNoSeed):
		log.Info("no NoCloud drive; running the stored configuration", "err", err)
		effective, applied, err = bootStored(host, configs, err, log)
	case err != nil:
		return store.Stored{}, fmt.Errorf("read the NoCloud drive: %w", err)
	default:
		effective, applied, err = bootSeeded(host, configs, userData, networkConfig, log)
	}
	if err != nil {
		return store.Stored{}, err
	}
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
	syncTime(host, cfg.Spec.Time, applied.NTP, log)
	if err := services.Prepare(); err != nil {
		return store.Stored{}, fmt.Errorf("prepare cgroups: %w", err)
	}
	if err := host.SealRoot(); err != nil {
		return store.Stored{}, fmt.Errorf("seal the root: %w", err)
	}
	log.Info("root remounted read-only")
	for _, svc := range cfg.Spec.Services {
		if err := services.Start(svc); err != nil {
			return store.Stored{}, fmt.Errorf("start %s: %w", svc.Name, err)
		}
	}
	saveAnchor(configs, cfg.Spec, log)
	return effective, nil
}

// bootSeeded brings the network up from the seed, mounts the data disk the seed's configuration
// names and chooses the effective configuration against the seed's digest. It stores the seed's
// network-config for a boot with no seed; a store that cannot take it is logged and the boot goes on.
func bootSeeded(host Host, configs Configs, userData, networkConfig []byte, log *slog.Logger) (store.Stored, netcfg.Applied, error) {
	base, err := config.Parse(userData)
	if err != nil {
		return store.Stored{}, netcfg.Applied{}, fmt.Errorf("invalid configuration: %w", err)
	}
	network, err := netcfg.Parse(networkConfig)
	if err != nil {
		return store.Stored{}, netcfg.Applied{}, fmt.Errorf("invalid network configuration: %w", err)
	}
	log.Info("configuration accepted", "node", base.Metadata.Name, "services", len(base.Spec.Services))

	applied, err := host.ApplyNetwork(network, log)
	if err != nil {
		return store.Stored{}, netcfg.Applied{}, fmt.Errorf("apply network configuration: %w", err)
	}
	log.Info("network configured", "interfaces", len(network.Interfaces), "nameservers", len(applied.Nameservers))

	if base.Spec.DataDisk.FormatBlank {
		if err := host.FormatBlankDataDisk(base.Spec.DataDisk, log); err != nil {
			return store.Stored{}, netcfg.Applied{}, err
		}
	}
	if err := mountData(host, configs, base.Spec.DataDisk, log); err != nil {
		return store.Stored{}, netcfg.Applied{}, err
	}
	effective := choose(configs, store.Digest(userData), base, log)
	if err := configs.SaveNetwork(networkConfig); err != nil {
		log.Error("store the network-config of the seed; a boot with no seed will not have it", "err", err)
	}
	return effective, applied, nil
}

// bootStored runs a Node with no seed: it mounts the data disk by its default label and mount
// point, never formatting it, loads the stored configuration and the stored network-config, and
// brings the network up from them. noSeed is the reason there is no seed, which a refusal names
// beside the reason there is no stored configuration. A stored configuration has no base to
// compare with, so its digest is not checked. A Node whose network-config was never stored, because
// it last booted with a seed before init stored it, is refused: no network is guessed.
func bootStored(host Host, configs Configs, noSeed error, log *slog.Logger) (store.Stored, netcfg.Applied, error) {
	disk := config.DataDisk{Label: wire.DataLabel, MountPoint: wire.DataMount}
	if err := mountData(host, configs, disk, log); err != nil {
		return store.Stored{}, netcfg.Applied{}, fmt.Errorf("%w, and the data disk %q that holds the stored configuration cannot be mounted: %w", noSeed, disk.Label, err)
	}
	stored, err := configs.Load()
	if err != nil {
		return store.Stored{}, netcfg.Applied{}, fmt.Errorf("%w, and there is no valid stored configuration: %w", noSeed, err)
	}
	networkConfig, err := configs.LoadNetwork()
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Stored{}, netcfg.Applied{}, fmt.Errorf("%w, and there is no stored network-config; attach the seed for one boot so that init stores it", noSeed)
	case err != nil:
		return store.Stored{}, netcfg.Applied{}, fmt.Errorf("%w, and the stored network-config cannot be read: %w", noSeed, err)
	}
	network, err := netcfg.Parse(networkConfig)
	if err != nil {
		return store.Stored{}, netcfg.Applied{}, fmt.Errorf("invalid stored network configuration: %w", err)
	}
	applied, err := host.ApplyNetwork(network, log)
	if err != nil {
		return store.Stored{}, netcfg.Applied{}, fmt.Errorf("apply network configuration: %w", err)
	}
	log.Info("network configured", "interfaces", len(network.Interfaces), "nameservers", len(applied.Nameservers))
	log.Info("running the stored configuration", "revision", stored.Revision)
	return stored, applied, nil
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

// syncTime sets the clock from spec.time once, before the services start. It never refuses
// the boot: a clock that no server could set is logged and the boot goes on, and the Node keeps
// trying (timesync.Syncer.Run). Without spec.time it uses the time servers the DHCPv4 lease named.
func syncTime(host Host, spec *config.Time, leased []netip.Addr, log *slog.Logger) {
	var servers []string
	switch {
	case spec != nil:
		servers = spec.Addrs()
	case len(leased) > 0:
		for _, a := range leased {
			servers = append(servers, net.JoinHostPort(a.String(), "123"))
		}
	default:
		log.Info("time is not synchronised: the configuration has no spec.time and the lease named no time server")
		return
	}
	res, err := host.SyncTime(servers)
	if err != nil {
		log.Warn("time not synchronised; the boot goes on and init keeps trying", "err", err)
		return
	}
	log.Info("time synchronised", "sync", res)
}

// choose returns the effective configuration: the stored one when its base is
// the user-data the Node holds now and it validates, and base, at revision 0,
// otherwise. A stored configuration that is not used is discarded and
// replaced by the base, with the reason logged. A store that cannot be read is
// kept: the base runs for this boot and nothing is stored. A Node with no store
// runs its base.
func choose(configs Configs, digest string, base config.NodeConfig, log *slog.Logger) store.Stored {
	fresh := store.Stored{Revision: 0, BaseSha256: digest, Config: base}
	stored, err := configs.Load()
	var reason string
	switch {
	case errors.Is(err, store.ErrNotFound):
		log.Info("no stored configuration; running the base", "revision", 0)
		return fresh
	case errors.Is(err, store.ErrUnreadable):
		log.Error("stored configuration kept; running the base as revision 0 for this boot and storing nothing", "err", err)
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
