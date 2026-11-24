package run

import (
	"context"
	"log/slog"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/boot"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/extension"
	"github.com/garamsh/naru/lib/init/linux"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/timesync"
)

// bootHost is the machine with the means to mount the system extensions.
type bootHost struct {
	*linux.Host
	extensions *extension.Manager
}

func (h bootHost) MountExtensions(specs []config.Extension) error { return h.extensions.Mount(specs) }

// timedHost is the machine as the boot sequence reaches it, with its clock.
type timedHost interface {
	boot.Host
	// RunTime keeps the clock in step with spec.time until ctx ends.
	RunTime(ctx context.Context, log *slog.Logger)
	// TimeStatus returns what the clock's last synchronisation did.
	TimeStatus() timesync.Status
	// RunNetwork keeps the DHCPv4 lease, when the network has one, until ctx ends.
	RunNetwork(ctx context.Context) error
}

// timeAPI is the Node API as the boot sequence starts it, and as it learns the clock's state.
type timeAPI interface {
	boot.API
	// UseTime has the API report the clock's state through status.
	UseTime(status func() timesync.Status)
}

// bootAndKeep runs the boot sequence, and then keeps the clock in step and the
// DHCPv4 lease for as long as the Node runs, whether the sequence finished or
// refused: a service that started before the refusal keeps running. It returns
// the function that stops the clock and the lease and waits for both to end, and
// the effective configuration when the sequence finished, or else the refusal.
func bootAndKeep(host timedHost, services boot.Services, configs boot.Configs, api timeAPI, log *slog.Logger) (stop func(), booted *store.Stored, refusal error) {
	api.UseTime(host.TimeStatus)
	stored, err := boot.Run(host, services, configs, api, log)
	if err != nil {
		refusal = err
		// Stay up so the reason stays on the console, and start nothing more.
		log.Error("boot refused; no service of the configuration will start", "err", err)
	} else {
		booted = &stored
	}
	running, cancel := context.WithCancel(context.Background())
	ticked := make(chan struct{})
	go func() {
		defer close(ticked)
		host.RunTime(running, log)
	}()
	leased := make(chan struct{})
	go func() {
		defer close(leased)
		if err := host.RunNetwork(running); err != nil {
			log.Error("the DHCPv4 lease is no longer kept", "err", err)
		}
	}()
	return func() {
		cancel()
		<-ticked
		<-leased
	}, booted, refusal
}

// hostParts is the machine as the apply engine reaches it.
type hostParts interface {
	apply.Files
	apply.Time
	apply.Routes
	apply.Registries
}

// newEngine returns the engine that applies changes to the Node that booted
// stored, acting through the Node's own parts.
func newEngine(stored store.Stored, configs apply.Store, services apply.Services, host hostParts, api apply.API) *apply.Engine {
	return apply.New(apply.Deps{Store: configs, Services: services, Files: host, Time: host, Routes: host, Registries: host, API: api}, stored)
}
