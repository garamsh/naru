// Command naru-standin is the service naru's QEMU tests configure in place of
// gaon's Node Agent: it reports that the data disk is mounted and that mvmd's
// socket accepts connections, then runs until stopped.
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
	flag.DurationVar(&opts.Timeout, "timeout", time.Minute, "how long to wait for the socket")
	flag.Parse()
	if opts.Socket == "" {
		slog.Error("-socket is required")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := standin.Run(ctx, opts, slog.New(slog.NewTextHandler(os.Stdout, nil)).With("service", "naru-standin")); err != nil {
		slog.Error("stand-in failed", "err", err)
		os.Exit(1)
	}
}
