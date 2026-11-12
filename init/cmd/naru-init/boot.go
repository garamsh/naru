package main

import (
	"context"
	"log/slog"

	"github.com/garamsh/naru/init/internal/apply"
	"github.com/garamsh/naru/init/internal/boot"
	"github.com/garamsh/naru/init/internal/pki"
	"github.com/garamsh/naru/init/internal/store"
	"github.com/garamsh/naru/init/internal/timesync"
)

// timedHost is the machine as the boot sequence reaches it, with its clock.
type timedHost interface {
	boot.Host
	// RunTime keeps the clock in step with spec.time until ctx ends.
	RunTime(ctx context.Context, log *slog.Logger)
	// TimeStatus returns what the clock's last synchronisation did.
	TimeStatus() timesync.Status
}

// timeAPI is the Node API as the boot sequence starts it, and as it learns the clock's state.
type timeAPI interface {
	boot.API
	// UseTime has the API report the clock's state through status.
	UseTime(status func() timesync.Status)
}

// bootAndRenew runs the boot sequence, and then renews the certificates it
// issued and keeps the clock in step for as long as the Node runs, whether the
// sequence finished or refused: a service that started before the refusal keeps
// running and keeps reading its certificate. It returns the function that stops
// the renewal and the clock and waits for a renewal in flight to end, and the
// effective configuration when the sequence finished, which is nil when it
// refused.
func bootAndRenew(host timedHost, issuer *pki.Issuer, services boot.Services, configs boot.Configs, api timeAPI, log *slog.Logger) (stop func(), booted *store.Stored) {
	api.UseTime(host.TimeStatus)
	stored, err := boot.Run(host, issuer, services, configs, api, log)
	if err != nil {
		// Stay up so the reason stays on the console, and start nothing more.
		log.Error("boot refused; no further service will start", "err", err)
	} else {
		booted = &stored
	}
	renewing, cancel := context.WithCancel(context.Background())
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		issuer.Run(renewing)
	}()
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		host.RunTime(renewing, log)
	}()
	return func() {
		cancel()
		<-renewed
		<-ticked
	}, booted
}

// hostParts is the machine as the apply engine reaches it.
type hostParts interface {
	apply.Files
	apply.Time
}

// newEngine returns the engine that applies changes to the Node that booted
// stored, acting through the Node's own parts.
func newEngine(stored store.Stored, configs apply.Store, services apply.Services, host hostParts, certificates apply.Certificates, api apply.API) *apply.Engine {
	return apply.New(apply.Deps{Store: configs, Services: services, Files: host, Time: host, Certificates: certificates, API: api}, stored)
}
