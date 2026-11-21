// Command naru-volumes is the CSI driver of the volume add-on (docs/architecture/adr/0083): it
// serves the identity, controller and node services of the Container Storage Interface on a unix
// socket, for the external-provisioner and the kubelet on the same Node.
package main

import (
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"

	"github.com/garamsh/naru/node/internal/volumes"
)

func main() {
	endpoint := flag.String("endpoint", "unix:///csi/csi.sock", "the unix socket to serve on")
	config := flag.String("config", "/etc/naru-volumes.json", "the driver's configuration file")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(*endpoint, *config, os.Getenv("NODE_NAME"), log); err != nil {
		log.Error("naru-volumes", "error", err)
		os.Exit(1)
	}
}

func run(endpoint, config, node string, log *slog.Logger) error {
	if node == "" {
		return errors.New("NODE_NAME is empty")
	}
	path, ok := strings.CutPrefix(endpoint, "unix://")
	if !ok {
		return errors.New("the endpoint must be unix://<path>")
	}
	cfg, err := volumes.LoadConfig(config)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	d := volumes.New(node, cfg, volumes.XFSQuota{}, volumes.BindMounter{}, volumes.SysDisks{})
	s := grpc.NewServer(grpc.ChainUnaryInterceptor(logCalls(log)))
	csi.RegisterIdentityServer(s, d)
	csi.RegisterControllerServer(s, d)
	csi.RegisterNodeServer(s, d)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-stop; s.GracefulStop() }()
	log.Info("serving", "endpoint", endpoint, "node", node)
	return s.Serve(l)
}
