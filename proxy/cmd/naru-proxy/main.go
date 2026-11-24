// Command naru-proxy terminates TLS for Machines' stable names and aliases on a proxy host and
// forwards each to its Machine over the host mesh (docs/architecture/proxy.md).
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/proxy/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		slog.Error("naru-proxy", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen       = flag.String("listen", "0.0.0.0:"+wire.ProxyPort, "the addresses TLS is terminated on, separated by commas")
		cert         = flag.String("cert", "", "the zone's wildcard certificate, PEM, then its chain")
		key          = flag.String("key", "", "the zone certificate's private key, PEM")
		routesFile   = flag.String("routes-file", wire.ProxyRoutesFile, "the routes file hostd writes")
		revisionFile = flag.String("revision-file", wire.ProxyRevisionFile, "where the revision of the table loaded is written, for hostd")
		healthAddr   = flag.String("health-addr", wire.ProxyHealthAddr, "where GET /healthz is answered")
		maxAge       = flag.Duration("route-max-age", time.Hour, "how long after hostd took a table it is forwarded on")
		reload       = flag.Duration("reload-interval", time.Second, "how often the routes file is checked for a new table")
	)
	flag.Parse()
	if *maxAge <= 0 || *reload <= 0 {
		return errors.New("--route-max-age and --reload-interval must be positive")
	}
	zone, err := tls.LoadX509KeyPair(*cert, *key)
	if err != nil {
		return fmt.Errorf("the zone's certificate: %w", err)
	}
	log := slog.Default()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	routes := proxy.NewRoutes(*routesFile, *revisionFile, log)
	srv := proxy.New(proxy.Config{Zone: zone, Routes: routes, MaxAge: *maxAge, Now: time.Now, Log: log})
	var listeners []net.Listener
	for addr := range strings.SplitSeq(*listen, ",") {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		listeners = append(listeners, l)
	}
	hl, err := net.Listen("tcp", *healthAddr)
	if err != nil {
		return err
	}
	health := &http.Server{Handler: srv.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = health.Serve(hl) }()
	defer func() { _ = health.Close() }()
	go routes.Watch(ctx, *reload)
	log.Info("serving", "listen", *listen, "health", *healthAddr, "routes", *routesFile)
	return srv.Serve(ctx, listeners...)
}
