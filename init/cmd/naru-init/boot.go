package main

import (
	"context"
	"log/slog"

	"github.com/garamsh/naru/init/internal/boot"
	"github.com/garamsh/naru/init/internal/pki"
)

// bootAndRenew runs the boot sequence, and then renews the certificates it
// issued for as long as the Node runs, whether the sequence finished or
// refused: a service that started before the refusal keeps running and keeps
// reading its certificate. It returns the function that stops the renewal and
// waits for a renewal in flight to end.
func bootAndRenew(host boot.Host, issuer *pki.Issuer, services boot.Services, log *slog.Logger) (stop func()) {
	if err := boot.Run(host, issuer, services, log); err != nil {
		// Stay up so the reason stays on the console, and start nothing more.
		log.Error("boot refused; no further service will start", "err", err)
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
	}
}
