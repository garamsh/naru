// Command naru-hostd is the host API of a Node (docs/architecture/host.md): it creates, reads and
// destroys microVMs through mvmd's local runtime API, and keeps mvmd's fencing deadline armed.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garamsh/naru/host/internal/api"
	"github.com/garamsh/naru/host/internal/enroll"
	"github.com/garamsh/naru/host/internal/fence"
	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/metrics"
)

func main() {
	var (
		listen   = flag.String("listen", "0.0.0.0:7443", "address of the host API")
		cert     = flag.String("cert", "/run/naru-hostd/tls.crt", "serving certificate")
		key      = flag.String("key", "/run/naru-hostd/tls.key", "serving key")
		clients  = flag.String("client-ca", "/run/naru-hostd/client-ca.crt", "CA of the clients' certificates")
		hostFile = flag.String("host-file", "/run/naru-hostd/host.json", "region and labels, written by init from spec.host")
		socket   = flag.String("mvmd-socket", "/run/mvmd.sock", "mvmd's runtime API socket")
		gateway  = flag.String("machine-gateway", "", "gateway of the microVMs' network, written into the seed of an OS-form microVM")
		length   = flag.Duration("fence-length", 10*time.Minute, "how long each renewal arms mvmd's fencing deadline")
		interval = flag.Duration("fence-interval", time.Minute, "how often the deadline is renewed")

		orchURL   = flag.String("orch-url", "", "https URL of naru-orchd; empty turns enrollment off")
		orchCA    = flag.String("orch-ca-sha256", "", "hex SHA-256 of the authority orch's certificate chains to")
		enrollKey = flag.String("enroll-key-file", "", "file holding the enrollment key")
		hostKey   = flag.String("host-key", "/var/lib/naru/hostd/host.key", "the host's own key, generated 0600 on first start; its certificate is written beside it")
		hostID    = flag.String("host-id", "", "the name this host enrolls under (default: the hostname)")
		advertise = flag.String("advertise", "", "address orch lists for this host (default: --listen)")

		metricsAddr = flag.String("metrics-addr", "", "address that serves /metrics over plain HTTP; empty serves none")

		management = flag.String("management-prefix", "", "prefix of the management plane; set, it turns the virtual networks on")
		hostAddrs  = flag.String("host-addrs", "", "comma-separated addresses of this host that no network may reach")
		tapPrefix  = flag.String("tap-prefix", "tap", "name prefix of the microVMs' tap devices, for the forged-source rule")
		egress     = flag.String("egress-interface", "", "interface the networks' egress leaves by; empty gives them none")
		tunnel     = flag.String("tunnel", "wg-naru", "name of the WireGuard interface, which must exist; empty turns the tunnel off")
		tunnelKey  = flag.String("tunnel-key", "/run/naru-hostd/wg.key", "file of the tunnel's private key, mode 0600")
		tunnelPort = flag.Int("tunnel-port", 51820, "UDP port the tunnel listens on")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	var nets api.Networks
	if *management != "" {
		m, err := networkManager(*management, *hostAddrs, *tapPrefix, *egress, *tunnel, *tunnelKey, *tunnelPort)
		if err != nil {
			log.Error("cannot set up the virtual networks", "err", err.Error())
			os.Exit(1)
		}
		nets = m
	}

	conf, err := api.TLSConfig(*cert, *key, *clients)
	if err != nil {
		log.Error("cannot load the serving pair or the client CA", "err", err.Error())
		os.Exit(1)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("cannot listen", "addr", *listen, "err", err.Error())
		os.Exit(1)
	}
	var gw netip.Addr
	if *gateway != "" {
		if gw, err = netip.ParseAddr(*gateway); err != nil || !gw.Is4() {
			log.Error("--machine-gateway is not an IPv4 address", "value", *gateway)
			os.Exit(1)
		}
	}
	rt := mvmd.New(*socket)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	r := &fence.Renewer{Runtime: rt, Length: *length, Interval: *interval, Retry: 5 * time.Second, Log: log}
	go r.Run(ctx)
	go func() { <-ctx.Done(); _ = ln.Close() }()

	if *orchURL != "" {
		id := *hostID
		if id == "" {
			if id, err = os.Hostname(); err != nil {
				log.Error("cannot read the hostname; pass --host-id", "err", err.Error())
				os.Exit(1)
			}
		}
		addr := *advertise
		if addr == "" {
			addr = *listen
		}
		agent := enroll.New(enroll.Config{OrchURL: *orchURL, CASHA256: *orchCA, EnrollKeyFile: *enrollKey, KeyFile: *hostKey, HostID: id, Address: addr, Out: os.Stdout, Log: log})
		go func() {
			if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("enrollment with orch ended", "err", err.Error())
			}
		}()
	}

	reg := metrics.New()
	srv := api.New(api.Deps{Runtime: rt, HostFile: *hostFile, MachineGateway: gw, Networks: nets, Log: log, Metrics: reg})
	if *metricsAddr != "" {
		go func() {
			if err := metrics.Serve(ctx, *metricsAddr, reg); err != nil {
				log.Error("the metrics listener stopped", "addr", *metricsAddr, "err", err.Error())
			}
		}()
	}

	log.Info("naru-hostd serving", "addr", *listen)
	if err := srv.Serve(ln, conf); err != nil && ctx.Err() == nil {
		log.Error("the host API stopped", "err", err.Error())
		os.Exit(1)
	}
}
