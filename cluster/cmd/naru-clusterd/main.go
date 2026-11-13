// Command naru-clusterd runs naru's cluster controllers (docs/architecture/cluster.md): it keeps
// NaruHost objects current from the host APIs and turns each NaruClusterDeclaration into the
// Cluster API objects of one cluster.
package main

import (
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/garamsh/naru/cluster/internal/declaration"
	"github.com/garamsh/naru/cluster/internal/host"
	"github.com/garamsh/naru/cluster/internal/scheme"
)

func main() {
	var (
		metrics  = flag.String("metrics-bind-address", ":8080", "address of the metrics endpoint; 0 turns it off")
		probes   = flag.String("health-probe-bind-address", ":8081", "address of the health probes")
		leader   = flag.Bool("leader-elect", false, "elect one active replica through a Lease")
		interval = flag.Duration("host-poll-interval", 30*time.Second, "how often each host API is asked")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme.New(),
		Metrics:                metricsserver.Options{BindAddress: *metrics},
		HealthProbeBindAddress: *probes,
		LeaderElection:         *leader,
		LeaderElectionID:       "naru-clusterd.naru.garam.sh",
	})
	if err != nil {
		fatal(log, "cannot create the manager", err)
	}
	if err := (&host.Reconciler{Client: mgr.GetClient(), Interval: *interval}).SetupWithManager(mgr, "naruhost"); err != nil {
		fatal(log, "cannot set up the NaruHost controller", err)
	}
	if err := (&declaration.Reconciler{Client: mgr.GetClient()}).SetupWithManager(mgr, "narucluster-declaration"); err != nil {
		fatal(log, "cannot set up the declaration controller", err)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		fatal(log, "cannot add the health check", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		fatal(log, "cannot add the readiness check", err)
	}
	log.Info("naru-clusterd starting")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fatal(log, "the manager stopped", err)
	}
}

func fatal(log *slog.Logger, msg string, err error) {
	log.Error(msg, "err", err.Error())
	os.Exit(1)
}
