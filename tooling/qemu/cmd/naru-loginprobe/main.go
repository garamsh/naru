// Command naru-loginprobe is the service naru's QEMU tests run to see that a Node has no login path
// (ADR 0047): every few seconds it reports the TCP ports the Node listens on, the shells and login
// programs in its file system and the processes that read a terminal, until it is stopped. It
// reports again because the Node's other services open their ports after it starts.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/loginprobe"
)

const interval = 5 * time.Second

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil)).With("service", "naru-loginprobe")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	opts := loginprobe.Options{
		Root: "/", Proc: "/proc",
		// The data disk carries the programs the tests stage, which are not the image's.
		Skip: []string{"/proc", "/sys", "/dev", "/run", "/var/lib/naru"},
	}
	for opts.Pass = 1; ; opts.Pass++ {
		if err := loginprobe.Run(opts, log); err != nil {
			log.Error("loginprobe failed", "err", err)
			os.Exit(1)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
