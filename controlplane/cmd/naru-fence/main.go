// Command naru-fence is the service a control-plane Node's configuration starts to close etcd's
// client and peer ports to every address but the control-plane Nodes' and the loopback
// (docs/architecture/adr/0024). It writes its netfilter table and waits for a signal, so that init,
// which restarts a service that exits, does not write it again; the table stays when it stops.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/nftables"

	"github.com/garamsh/naru/controlplane/internal/fence"
)

func main() {
	allow := flag.String("allow", "", "comma-separated IPv4 addresses that may reach etcd's ports; 127.0.0.1 is always allowed")
	machines := flag.String("allow-machines", "", "comma-separated IPv4 prefixes of this Node's MicroVMs: their connections to etcd's client port are accepted, and their packets to its peer port are rejected where this Node sends them")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(*allow, *machines); err != nil {
		log.Error("naru-fence", "err", err)
		os.Exit(1)
	}
	log.Info("naru-fence: etcd's ports are closed to addresses outside the control plane", "allow", *allow)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
}

func run(allow, machines string) error {
	addrs := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
	for _, s := range strings.Split(allow, ",") {
		if s == "" {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("-allow: %w", err)
		}
		addrs = append(addrs, a)
	}
	allowed, err := fence.Allowed(addrs)
	if err != nil {
		return err
	}
	var prefixes []netip.Prefix
	for _, s := range strings.Split(machines, ",") {
		if s == "" {
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return fmt.Errorf("-allow-machines: %w", err)
		}
		prefixes = append(prefixes, p)
	}
	c, err := nftables.New()
	if err != nil {
		return err
	}
	return fence.Apply(c, allowed, prefixes)
}
