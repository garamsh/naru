// Command naru-vip is the service a control-plane Node's configuration starts to hold the control
// plane's virtual address (docs/architecture/adr/0024): it looks at the API server of every
// control-plane Node, and keeps the address on this Node exactly while this Node is the first of
// them, in the order -member lists them, whose API server answers over TLS and whose etcd member is healthy.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/garamsh/naru/lib/vip"
)

func main() {
	var (
		address = flag.String("address", "", "the virtual address with its prefix length, such as 192.168.0.41/24")
		link    = flag.String("interface", "eth0", "the network card that carries the address")
		self    = flag.String("self", "", "this Node's own address, one of -member")
		members = flag.String("member", "", "comma-separated addresses of the control-plane Nodes, in priority order")
		api     = vip.API{}
		etcd    = vip.Etcd{}
	)
	flag.StringVar(&api.Port, "port", "6443", "the API server's port, which must answer an HTTP request over TLS for a member to be alive")
	flag.StringVar(&etcd.Port, "etcd-port", "2379", "etcd's client port, whose /health must answer for a member to be alive")
	flag.StringVar(&etcd.CAFile, "etcd-ca-file", "", "the CA that signed the serving certificates of etcd and of the API server")
	flag.StringVar(&etcd.CertFile, "etcd-cert-file", "", "the client certificate etcd accepts")
	flag.StringVar(&etcd.KeyFile, "etcd-key-file", "", "the key of -etcd-cert-file")
	flag.Parse()
	api.CAFile = etcd.CAFile
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if etcd.CAFile == "" || etcd.CertFile == "" || etcd.KeyFile == "" {
		log.Error("naru-vip", "err", "-etcd-ca-file, -etcd-cert-file and -etcd-key-file are required")
		os.Exit(2)
	}
	cfg, host, err := parse(*address, *link, *self, *members)
	if err != nil {
		log.Error("naru-vip", "err", err)
		os.Exit(2)
	}
	api.Name = host.Address.Addr().String()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log.Info("naru-vip: started", "address", *address, "self", *self, "members", *members)
	vip.Run(ctx, cfg, host, vip.LiveProbe(api, etcd, time.Second), log)
}

func parse(address, link, self, members string) (vip.Config, vip.Linux, error) {
	prefix, err := netip.ParsePrefix(address)
	if err != nil || !prefix.Addr().Is4() {
		return vip.Config{}, vip.Linux{}, fmt.Errorf("-address %q is not an IPv4 address with a prefix length", address)
	}
	cfg := vip.Config{Interval: time.Second, Misses: 3, AnnounceEvery: 10, Settle: 5}
	if cfg.Self, err = netip.ParseAddr(self); err != nil {
		return cfg, vip.Linux{}, fmt.Errorf("-self: %w", err)
	}
	found := false
	for _, s := range strings.Split(members, ",") {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return cfg, vip.Linux{}, fmt.Errorf("-member: %w", err)
		}
		found = found || a == cfg.Self
		cfg.Members = append(cfg.Members, a)
	}
	if !found {
		return cfg, vip.Linux{}, fmt.Errorf("-self %s is not one of -member", self)
	}
	return cfg, vip.Linux{Link: link, Address: prefix}, nil
}
