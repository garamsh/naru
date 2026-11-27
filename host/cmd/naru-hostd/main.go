// Command naru-hostd is the host API of a Node (docs/architecture/host.md): it creates, reads and
// destroys microVMs through mvmd's local runtime API, and keeps mvmd's fencing deadline armed. With
// --join it is the installer's enrollment agent instead and serves nothing.
package main

import (
	"context"
	"errors"
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
	"github.com/garamsh/naru/host/internal/hardware"
	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/metrics"
	"github.com/garamsh/naru/lib/wire"
)

func main() {
	var (
		listen   = flag.String("listen", "0.0.0.0:7443", "address of the host API")
		cert     = flag.String("cert", "/run/naru-hostd/tls.crt", "serving certificate")
		key      = flag.String("key", "/run/naru-hostd/tls.key", "serving key")
		clients  = flag.String("client-ca", "/run/naru-hostd/client-ca.crt", "CA of the clients' certificates")
		hostFile = flag.String("host-file", "/run/naru-hostd/host.json", "region and labels, written by init from spec.host")
		initFile = flag.String("init-file", "/run/naru-hostd/init.json", "the version, slot and last upgrade of the naru the host runs, written by init")
		socket   = flag.String("mvmd-socket", "/run/mvmd.sock", "mvmd's runtime API socket")
		gateway  = flag.String("machine-gateway", "", "gateway of the microVMs' network, written into the seed of an OS-form microVM")
		length   = flag.Duration("fence-length", 10*time.Minute, "how long each renewal arms mvmd's fencing deadline")
		interval = flag.Duration("fence-interval", time.Minute, "how often the deadline is renewed")

		orchURL   = flag.String("orch-url", "", "https URL of naru-orchd; empty turns enrollment off")
		orchCA    = flag.String("orch-ca-sha256", "", "hex SHA-256 of the authority orch's certificate chains to")
		enrollKey = flag.String("enroll-key-file", "", "file holding the enrollment key")
		joinKey   = flag.String("enroll-key", "", "file holding the enrollment key, for --join (the install seed's)")
		join      = flag.Bool("join", false, "enroll from the install seed, receive the configuration orch built, write it to --join-config and exit; serves no API")
		joinConf  = flag.String("join-config", "/var/lib/naru/join/config.yaml", "where --join writes the configuration orch answered with, mode 0600")
		hostKey   = flag.String("host-key", "/var/lib/naru/hostd/host.key", "the host's own key, generated 0600 on first start; its certificate is written beside it")
		hostID    = flag.String("host-id", "", "the name this host enrolls under (default: the hostname)")
		advertise = flag.String("advertise", "", "address orch lists for this host (default: --listen)")

		revKey      = flag.String("revocation-public-key", "", "PEM public key that verifies the signer's revocation list; empty checks no revocation")
		revMaxAge   = flag.Duration("revocation-max-age", identity.DefaultMaxAge, "how long a revocation list that cannot be refreshed is used before every client is refused")
		revSocket   = flag.String("revocation-signer-socket", "", "the local signer's socket to read the list from; empty fetches it from --orch-url")
		tokenFile   = flag.String("leader-token-file", "/var/lib/naru/hostd/leader-token", "where the highest leader token orch has sent is kept, so a restart does not forget it")
		revFile     = flag.String("revocation-file", "/var/lib/naru/hostd/crl.json", "where the last verified list is kept, so a restart within the max age needs no fetch first")
		revSeed     = flag.String("revocation-seed", "", "a signed list delivered with the host's configuration, taken when it is newer than the kept one")
		maintenance = flag.Bool("maintenance", false, "serve only the maintenance report and the reset, for a host whose boot init refused (ADR 0082); needs --orch-url, --orch-ca-sha256 and --revocation-public-key")
		maintReason = flag.String("maintenance-reason", "", "why the boot was refused, as init says it, reported by GET /v1/maintenance")
		resetMarker = flag.String("reset-marker", wire.ResetMarker, "the file a reset leaves for init, which carries it out at the next boot")
		metricsAddr = flag.String("metrics-addr", "", "address that serves /metrics over plain HTTP; empty serves none")

		management = flag.String("management-prefix", "", "prefix of the management plane; set, it turns the virtual networks on")
		tapPrefix  = flag.String("tap-prefix", "tap", "name prefix of the microVMs' tap devices, for the forged-source rule")
		egress     = flag.String("egress-interface", "", "interface the networks' egress leaves by; empty gives them none")
		tunnel     = flag.String("tunnel", "wg-naru", "name of the WireGuard interface, which hostd creates; empty turns the tunnel off")
		tunnelKey  = flag.String("tunnel-key", "/var/lib/naru/hostd/wg.key", "the tunnel's private key, generated 0600 on first start")
		netState   = flag.String("networks-state", "/var/lib/naru/hostd/networks.json", "file the networks, members and peers are kept in across restarts, mode 0600")
		lan        = flag.String("lan-interface", "", "interface the host announces exposed LAN addresses on (ADR 0085); empty exposes none; needs --management-prefix")
		tunnelPort = flag.Int("tunnel-port", 51820, "UDP port the tunnel listens on")

		proxyHealth = flag.String("proxy-health-addr", "", "the health endpoint of the host's naru-proxy, which makes the host a candidate for a proxy exposure (ADR 0112); empty means the host runs none")
		proxyAge    = flag.Duration("proxy-route-max-age", wire.ProxyRouteMaxAge, "the proxy's --route-max-age, which GET /v1/host reports so orch waits it out before it reuses an address a table named")
		proxyTLS    = flag.Int("proxy-tls-port", 0, "the port of the proxy's TLS listener, which a proxy exposure's 443 is redirected to; needs --proxy-health-addr")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if *join {
		os.Exit(runJoin(log, joinOptions{orchURL: *orchURL, orchCA: *orchCA, enrollKey: *joinKey, hostKey: *hostKey, config: *joinConf}))
	}

	if *maintenance {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		err := runMaintenance(ctx, maintenanceOptions{
			listen: *listen, orchURL: *orchURL, orchCA: *orchCA, hostKey: *hostKey, hostID: *hostID, reason: *maintReason, resetMarker: *resetMarker, reboot: rebootInit,
			revocation: revocationConfig{keyFile: *revKey, maxAge: *revMaxAge, orchURL: *orchURL, orchCA: *orchCA, file: *revFile},
		}, log)
		if err != nil && ctx.Err() == nil {
			log.Error("maintenance mode stopped", "err", err.Error())
			os.Exit(1)
		}
		return
	}

	proxyPorts, err := proxyFlags(*proxyHealth, *proxyTLS)
	if err == nil && *proxyAge <= 0 {
		err = errors.New("--proxy-route-max-age must be positive")
	}
	if err != nil {
		log.Error(err.Error())
		os.Exit(1)
	}
	var proxyState *proxyCheck
	var proxyHealthy func(context.Context) bool
	var routes *api.Routes
	if proxyPorts.Runs() {
		proxyState = &proxyCheck{}
		proxyHealthy = proxyState.Healthy
		routes = &api.Routes{File: wire.ProxyRoutesFile, GID: int(wire.ProxyUID), RevisionFile: wire.ProxyRevisionFile, MaxAge: *proxyAge}
	}
	if *lan != "" && *management == "" {
		log.Error("--lan-interface needs --management-prefix: the exposed addresses are forwarded to a network's members")
		os.Exit(1)
	}
	var nets api.Networks
	var netMgr *network.Manager
	if *management != "" {
		m, err := networkManager(context.Background(), networkConfig{
			management: *management, tapPrefix: *tapPrefix, egress: *egress,
			tunnel: *tunnel, tunnelKey: *tunnelKey, tunnelPort: *tunnelPort, state: *netState, lan: *lan, proxy: proxyPorts,
		})
		if err != nil {
			log.Error("cannot set up the virtual networks", "err", err.Error())
			os.Exit(1)
		}
		nets, netMgr = m, m
	}

	revocations, err := newRevocations(revocationConfig{
		keyFile: *revKey, maxAge: *revMaxAge, signerSocket: *revSocket, orchURL: *orchURL, orchCA: *orchCA, file: *revFile, seedFile: *revSeed,
	}, log)
	if err != nil {
		log.Error("cannot set up the revocation list", "err", err.Error())
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

	go prefetch(ctx, revocations, log)
	serving, err := chooseServing(ctx, revocations, log, *cert, *key, storedServingPath(*hostKey))
	if err != nil {
		log.Error("refusing to serve with a revoked key", "err", err.Error())
		os.Exit(1)
	}
	conf, err := api.TLSConfig(serving, *clients, revocations)
	if err != nil {
		log.Error("cannot load the client CA", "err", err.Error())
		os.Exit(1)
	}
	ownFiles := func() []string { return []string{serving.File(), enroll.CertPathFor(*hostKey)} }
	if err := checkOwnCertificates(ctx, revocations, ownFiles()...); errors.Is(err, identity.ErrRevoked) {
		log.Error("refusing to serve with a revoked key", "err", err.Error())
		os.Exit(1)
	}
	go refuseRevokedKey(ctx, revocations, ownCertificateInterval, log, func(err error) {
		log.Error("refusing to serve with a revoked key", "err", err.Error())
		os.Exit(1)
	}, ownFiles)

	r := &fence.Renewer{Runtime: rt, Length: *length, Interval: *interval, Retry: 5 * time.Second, Log: log}
	go r.Run(ctx)
	if proxyState != nil {
		go proxyState.run(ctx, probeProxy(*proxyHealth), proxyProbeInterval)
	}
	if netMgr != nil {
		startReconciler(ctx, netMgr, rt, log)
	}
	var exposures api.Exposures
	if *lan != "" {
		x, err := startExposer(ctx, netMgr, *lan, log)
		if err != nil {
			log.Error("cannot set up the exposures", "err", err.Error())
			os.Exit(1)
		}
		exposures = x
	}

	var hostPair api.Identity
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
		hostPair = agent
		go func() {
			if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
				log.Error("enrollment with orch ended", "err", err.Error())
			}
		}()
	}

	reg := metrics.New()
	resetID, err := hostIdentity(*hostID, *hostKey)
	if err != nil {
		log.Warn("cannot tell the host's ID; the reset call is off", "err", err.Error())
	}
	virt, err := hardware.Live().Virtualization()
	if err != nil {
		log.Error("cannot tell whether the host runs nested; it reports no virtualization label", "err", err.Error())
	}
	var importer *api.Importer
	if hostPair != nil && netMgr != nil {
		if importer, err = newImporter(*hostKey, *clients, *listen, netMgr.Peers); err != nil {
			log.Error("cannot set up the import of disks", "err", err.Error())
			os.Exit(1)
		}
	}
	// A reset of the data disk deletes serving.pem, so the pair that serves after it is the configuration's.
	srv := api.New(api.Deps{Runtime: rt, HostFile: *hostFile, InitFile: *initFile, MachineGateway: gw, Networks: nets, Exposures: exposures, LANInterface: *lan, ProxyHealthy: proxyHealthy, Routes: routes, Virtualization: virt, Log: log, Metrics: reg, Revocations: revocations, LeaderTokenFile: *tokenFile, Identity: hostPair, Serving: serving,
		HostID: resetID, ResetMarker: *resetMarker, Reboot: rebootInit, CheckPair: pairAfterReset(revocations, *cert), Importer: importer})
	if *metricsAddr != "" {
		go func() {
			if err := metrics.Serve(ctx, *metricsAddr, reg); err != nil {
				log.Error("the metrics listener stopped", "addr", *metricsAddr, "err", err.Error())
			}
		}()
	}

	log.Info("naru-hostd serving", "addr", *listen)
	if err := srv.Serve(ctx, ln, conf); err != nil {
		log.Error("the host API stopped", "err", err.Error())
		os.Exit(1)
	}
}
