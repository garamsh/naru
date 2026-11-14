package main

import (
	"fmt"
	"net/netip"
	"os"
	"strings"

	"github.com/garamsh/naru/host/internal/network"
)

// networkManager builds the manager of the virtual networks from the flags. The tunnel's key file
// must exist with no access for group or others; its contents are never read here.
func networkManager(management, hostAddrs, tapPrefix, egress, tunnel, keyFile string, port int) (*network.Manager, error) {
	mgmt, err := netip.ParsePrefix(management)
	if err != nil {
		return nil, fmt.Errorf("--management-prefix: %w", err)
	}
	var addrs []netip.Addr
	for _, s := range strings.Split(hostAddrs, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("--host-addrs: %w", err)
		}
		addrs = append(addrs, a)
	}
	if tunnel != "" {
		st, err := os.Stat(keyFile)
		if err != nil {
			return nil, fmt.Errorf("--tunnel-key: %w", err)
		}
		if st.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("--tunnel-key %s must have mode 0600, has %v", keyFile, st.Mode().Perm())
		}
	}
	return &network.Manager{
		Run: network.ExecRunner{}, KeyFile: keyFile, Port: port,
		Base: network.Config{Management: mgmt, HostAddrs: addrs, TapPrefix: tapPrefix, Egress: egress, Tunnel: tunnel},
	}, nil
}
