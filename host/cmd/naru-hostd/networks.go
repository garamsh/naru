package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/host/internal/network"
)

// networkConfig is what the flags say of the virtual networks.
type networkConfig struct {
	management, tapPrefix, egress string
	tunnel, tunnelKey             string
	tunnelPort                    int
	state                         string
	lan                           string
	proxy                         network.ProxyPorts
}

// networkManager builds the manager of the virtual networks from the flags, makes the tunnel
// interface (with a key generated on the first start), and applies the state the last run left, so
// the host is closed before the API serves.
func networkManager(ctx context.Context, c networkConfig) (*network.Manager, error) {
	mgmt, err := netip.ParsePrefix(c.management)
	if err != nil {
		return nil, fmt.Errorf("--management-prefix: %w", err)
	}
	m := &network.Manager{
		Run: network.ExecRunner{}, State: c.state,
		Base: network.Config{Management: mgmt, TapPrefix: c.tapPrefix, Egress: c.egress, LAN: c.lan, Proxy: c.proxy},
	}
	if c.tunnel != "" {
		if m.Tunnel, err = network.NewWireGuard(c.tunnel, c.tunnelKey, c.tunnelPort); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := m.Load(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// reconcileInterval is how often the networks' members are made to match mvmd's machines.
const reconcileInterval = 30 * time.Second

// reconcilePassTimeout bounds a pass, which holds the networks' lock while it asks mvmd.
const reconcilePassTimeout = 10 * time.Second

// startReconciler drops the members whose machine mvmd no longer holds, once before the API serves
// and then every reconcileInterval. A pass that cannot reach mvmd is logged, and the next one tries.
func startReconciler(ctx context.Context, m *network.Manager, rt mvmd.Runtime, log *slog.Logger) {
	r := &network.Reconciler{Manager: m, Addresses: machineAddresses(rt), Interval: reconcileInterval, Timeout: reconcilePassTimeout, Log: log}
	if err := r.Once(ctx); err != nil {
		log.Error("cannot reconcile the networks' members with the machines", "err", err.Error())
	}
	go r.Run(ctx)
}

// machineAddresses lists the addresses of the machines mvmd holds.
func machineAddresses(rt mvmd.Runtime) func(context.Context) ([]netip.Addr, error) {
	return func(ctx context.Context) ([]netip.Addr, error) {
		ms, err := rt.ListMachines(ctx)
		if err != nil {
			return nil, err
		}
		var out []netip.Addr
		for _, m := range ms {
			for _, s := range m.Addresses {
				if a, err := netip.ParseAddr(s); err == nil {
					out = append(out, a)
				}
			}
		}
		return out, nil
	}
}

// startExposer brings up the host's side of the exposures: it takes off the LAN interface the
// addresses an earlier run left that no lease holds, brings up those that do, and then drops the
// leases that run out and announces the addresses every second. It runs once before the API serves.
func startExposer(ctx context.Context, m *network.Manager, lan string, log *slog.Logger) (*network.Exposer, error) {
	link, err := network.NewLAN(lan)
	if err != nil {
		return nil, fmt.Errorf("--lan-interface: %w", err)
	}
	x := &network.Exposer{Manager: m, Link: link, Log: log}
	// A lease or an address that cannot be restored is logged and tried again every second; it does
	// not keep the host from serving the others.
	if err := x.Restore(ctx); err != nil {
		log.Error("cannot restore the exposures", "err", err.Error())
	}
	go x.Run(ctx)
	return x, nil
}
