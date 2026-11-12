package main

import (
	"context"
	"log/slog"

	"github.com/garamsh/naru/init/internal/apply"
	"github.com/garamsh/naru/init/internal/boot"
	"github.com/garamsh/naru/init/internal/pki"
	"github.com/garamsh/naru/init/internal/store"
)

// bootAndRenew runs the boot sequence, and then renews the certificates it
// issued for as long as the Node runs, whether the sequence finished or
// refused: a service that started before the refusal keeps running and keeps
// reading its certificate. It returns the function that stops the renewal and
// waits for a renewal in flight to end, and the effective configuration when
// the sequence finished, which is nil when it refused.
func bootAndRenew(host boot.Host, issuer *pki.Issuer, services boot.Services, configs boot.Configs, api boot.API, log *slog.Logger) (stop func(), booted *store.Stored) {
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
	return func() {
		cancel()
		<-renewed
	}, booted
}

// newEngine returns the engine that applies changes to the Node that booted
// stored, acting through the Node's own parts.
func newEngine(stored store.Stored, configs apply.Store, services apply.Services, files apply.Files, certificates apply.Certificates, api apply.API) *apply.Engine {
	return apply.New(apply.Deps{Store: configs, Services: services, Files: files, Certificates: certificates, API: api}, stored)
}
