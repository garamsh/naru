// Command naru-init is PID 1 of a naru Node. It mounts the pseudo file
// systems, reads the configuration, starts and supervises the services, reaps
// every orphan, and powers the machine off or reboots it on a signal.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/garamsh/naru/init/internal/console"
	"github.com/garamsh/naru/init/internal/linux"
	"github.com/garamsh/naru/init/internal/pki"
	"github.com/garamsh/naru/init/internal/service"
	"github.com/garamsh/naru/init/internal/store"
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
	stopRenewing, booted := bootAndRenew(host, issuer, services, configs, log)
	if booted != nil {
		engine := newEngine(*booted, configs, services, host, issuer)
		revision, _ := engine.Config()
		log.Info("the apply engine is ready", "revision", revision)
	}

	sig := <-sigs
	// A renewal in flight must not restart a service after it is stopped.
	stopRenewing()
	log.Info("stopping services", "signal", sig.String())
	services.StopAll()
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
