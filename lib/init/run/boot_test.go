package run

import (
	"context"
	"errors"
	"log/slog"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/netcfg"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/timesync"
)

const nodeNetwork = `version: 1
config:
  - type: physical
    name: eth0
    mac_address: '02:00:00:00:00:01'
    subnets:
      - type: static
        address: '10.78.0.1'
        netmask: '255.255.255.0'
`

// node is the Host and the Services of a Node whose second service cannot start.
type node struct {
	userData string
}

func (n *node) ReadSeed() ([]byte, []byte, error) {
	return []byte(n.userData), []byte(nodeNetwork), nil
}
func (n *node) ApplyNetwork(netcfg.Config, *slog.Logger) (netcfg.Applied, error) {
	return netcfg.Applied{}, nil
}
func (n *node) RunNetwork(context.Context) error    { return nil }
func (n *node) MountDataDisk(config.DataDisk) error { return nil }
func (n *node) FormatBlankDataDisk(config.DataDisk, *slog.Logger) error {
	return nil
}
func (n *node) EnableForwarding([]config.Family) error {
	return nil
}
func (n *node) AddRoute(config.Route) error                       { return nil }
func (n *node) DeleteRoute(config.Route) error                    { return nil }
func (n *node) WriteFiles([]config.File) error                    { return nil }
func (n *node) WriteRegistries(config.Spec) error                 { return nil }
func (n *node) PrepareStorage(config.Storage, *slog.Logger) error { return nil }
func (n *node) MountExtensions([]config.Extension) error          { return nil }
func (n *node) SealRoot() error                                   { return nil }
func (n *node) ClearDataDisk() error                              { return nil }
func (n *node) WipeDataDisk(config.DataDisk) error                { return nil }
func (n *node) SyncTime([]string) (timesync.Result, error) {
	return timesync.Result{}, nil
}
func (n *node) RunTime(ctx context.Context, _ *slog.Logger) { <-ctx.Done() }
func (n *node) TimeStatus() timesync.Status                 { return timesync.Status{} }
func (n *node) Prepare() error                              { return nil }
func (n *node) Restart(string)                              {}
func (n *node) Start(svc config.Service) error {
	if svc.Name == "b" {
		return errors.New("b cannot start")
	}
	return nil
}

// noAPI is a Node API that is never served.
type noAPI struct{}

func (noAPI) Start(store.Stored) error { return nil }

func (noAPI) UseTime(func() timesync.Status) {}
