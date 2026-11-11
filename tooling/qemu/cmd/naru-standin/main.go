// Command naru-standin is the service naru's QEMU tests configure in place of
// gaon's Node Agent and control plane: it reports that the data disk is mounted,
// that mvmd's socket accepts connections and that a peer Node's stand-in does,
// then runs until stopped.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/standin"
)

func main() {
	var opts standin.Options
	flag.StringVar(&opts.Socket, "socket", "", "Unix socket that must accept a connection")
	flag.StringVar(&opts.Mount, "mount", "", "directory to report as mounted")
	flag.StringVar(&opts.Listen, "listen", "", "TCP address to accept connections on")
	flag.StringVar(&opts.Peer, "peer", "", "TCP address of a peer stand-in that must accept a connection")
	flag.DurationVar(&opts.Timeout, "timeout", time.Minute, "how long to wait for the socket and the peer")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := standin.Run(ctx, opts, slog.New(slog.NewTextHandler(os.Stdout, nil)).With("service", "naru-standin")); err != nil {
		slog.Error("stand-in failed", "err", err)
		os.Exit(1)
	}
}
