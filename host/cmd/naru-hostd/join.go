package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/garamsh/naru/host/internal/enroll"
	"github.com/garamsh/naru/host/internal/hardware"
	"github.com/garamsh/naru/lib/orchapi"
)

type joinOptions struct {
	orchURL, orchCA, enrollKey, hostKey, config string
}

// runJoin enrolls the host from the install seed and returns the process's exit status: 0 once the
// configuration is written, 1 when it cannot be.
func runJoin(log *slog.Logger, o joinOptions) int {
	if o.orchURL == "" || o.orchCA == "" || o.enrollKey == "" {
		log.Error("--join needs --orch-url, --orch-ca-sha256 and --enroll-key")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	live := hardware.Live()
	agent := enroll.New(enroll.Config{
		OrchURL: o.orchURL, CASHA256: o.orchCA, EnrollKeyFile: o.enrollKey, KeyFile: o.hostKey,
		Join: true, JoinConfigFile: o.config,
		Hardware: func() (*orchapi.Hardware, error) { return live.Collect() },
		Out:      os.Stdout, Log: log,
	})
	if err := agent.Run(ctx); err != nil {
		log.Error("the host could not join", "err", err.Error())
		return 1
	}
	return 0
}
