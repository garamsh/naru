// Command naru-init is PID 1 of a naru Node. It mounts the pseudo file
// systems, reads the configuration, starts and supervises the services, reaps
// every orphan, powers the machine off on SIGTERM, and reboots it on SIGINT or
// when the Node API accepts a reboot.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/garamsh/naru/init/internal/api"
	"github.com/garamsh/naru/init/internal/console"
	"github.com/garamsh/naru/init/internal/linux"
	"github.com/garamsh/naru/init/internal/pki"
	"github.com/garamsh/naru/init/internal/service"
	"github.com/garamsh/naru/init/internal/store"
	"github.com/garamsh/naru/init/internal/upgrade"
)

const (
	cgroupRoot = "/sys/fs/cgroup"
	// diskWait is how long init waits for a disk the kernel has not yet probed.
	diskWait = 10 * time.Second
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "-validate" {
		os.Exit(validate(os.Args[2], os.Stdout, os.Stderr))
	}
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "naru-init must run as PID 1")
		os.Exit(2)
	}
	// Everything init and the services write to the console is kept, and re-printed at
	// intervals, for a reader that attaches after boot.
	con := console.New(os.Stderr, console.KeptBytes)
	ticker := time.NewTicker(console.Interval)
	defer ticker.Stop()
	go con.Run(context.Background(), ticker.C)
	log := slog.New(slog.NewTextHandler(con, nil))

	// Subscribe before anything is started, so a signal sent during boot is held.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	// The first thing, before anything is mounted: pivot_root, which the VMM supervisor does, fails on the rootfs.
	if err := linux.LeaveRootfs(); err != nil {
		log.Error("leave the rootfs", "err", err)
	}
	if err := linux.MountPseudoFilesystems(); err != nil {
		log.Error("mount pseudo file systems", "err", err)
	}

	reaper := linux.NewReaper()
	go reaper.Run(context.Background())

	host := linux.NewHost(diskWait)
	services := service.NewManager(linux.NewCgroupTree(cgroupRoot), linux.NewLauncher(reaper, con), service.SystemClock{}, log)

	issuer := pki.New("", service.SystemClock{}, services, log)
	configs := store.New(store.Dir)
	started := time.Now()
	// A reboot asked for through the API, and a trial boot that gives up, stop the Node the way
	// SIGINT does.
	reboot := func() {
		select {
		case sigs <- syscall.SIGINT:
		default:
		}
	}
	upgrades := upgrade.New(version(), started, log)
	upgrades.Detect()
	server := api.New(api.Deps{
		Services: services,
		Logs:     con,
		Version:  version(),
		BootID:   func() (string, error) { return linux.ReadBootID(linux.BootIDPath) },
		Uptime:   func() time.Duration { return time.Since(started) },
		Reboot:   reboot,
		Upgrades: upgrades,
		Log:      log,
	})
	stopRenewing, booted := bootAndRenew(host, issuer, services, configs, server, log)
	if booted != nil {
		server.Attach(newEngine(*booted, configs, services, host, issuer, server))
		log.Info("the apply engine is ready", "revision", booted.Revision)
	}
	// A trial boot of an upgrade is judged, and a default boot settles an upgrade left unfinished.
	judging, stopJudging := context.WithCancel(context.Background())
	if upgrades.Slot() == upgrade.SlotTrial {
		go upgrades.Watch(judging, booted != nil, services, reboot)
	} else {
		upgrades.Reconcile()
	}

	sig := <-sigs
	stopJudging()
	// A renewal in flight must not restart a service after it is stopped.
	stopRenewing()
	log.Info("stopping services", "signal", sig.String())
	services.StopAll()
	if err := server.Close(); err != nil {
		log.Error("close the Node API", "err", err)
	}
	switch sig {
	case syscall.SIGINT:
		err := host.Reboot()
		log.Error("reboot", "err", err)
	default:
		err := host.PowerOff()
		log.Error("power off", "err", err)
	}
	select {}
}

// buildVersion is the release version the image build gives init with -ldflags -X; it is empty in
// any other build.
var buildVersion string

// version returns the version init reports on the Node API: buildVersion, else the module version
// init was built at, else the commit it was built from, else "devel".
func version() string {
	if buildVersion != "" {
		return buildVersion
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "devel"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			return s.Value
		}
	}
	return "devel"
}
