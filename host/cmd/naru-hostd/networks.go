package main

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/garamsh/naru/host/internal/network"
)

// networkConfig is what the flags say of the virtual networks.
type networkConfig struct {
	management, tapPrefix, egress string
	tunnel, tunnelKey             string
	tunnelPort                    int
	state                         string
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
		Base: network.Config{Management: mgmt, TapPrefix: c.tapPrefix, Egress: c.egress},
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
