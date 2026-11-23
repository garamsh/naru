// Package run is the body of naru's PID 1, which the binaries of host/ and node/ call from their
// main. It mounts the pseudo file systems, reads the configuration, starts and supervises the
// services, reaps every orphan, powers the machine off on SIGTERM, and reboots it on SIGINT or when
// the Node API accepts a reboot.
package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/garamsh/naru/lib/init/api"
	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/boot"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/console"
	"github.com/garamsh/naru/lib/init/extension"
	"github.com/garamsh/naru/lib/init/linux"
	"github.com/garamsh/naru/lib/init/pki"
	"github.com/garamsh/naru/lib/init/service"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/upgrade"
)

const (
	cgroupRoot = "/sys/fs/cgroup"
	// diskWait is how long init waits for a disk the kernel has not yet probed.
	diskWait = 10 * time.Second
	// extensionProbeDir is where init mounts a new extension image for the moment it takes to read its manifest.
	extensionProbeDir = "/run/naru/extension-probe"
)

// Main runs PID 1 and never returns; given -validate <file> it checks a configuration and exits.
func Main() {
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
	ignoreSignals()

	// Ctrl-Alt-Del, the stop request mvmd sends a Firecracker guest, reaches init as SIGINT, which
	// reboots through the one stop path, rather than restarting the kernel with nothing stopped.
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_CAD_OFF); err != nil {
		log.Warn("make ctrl-alt-del a signal", "err", err)
	}

	// The first thing, before anything is mounted: pivot_root, which the VMM supervisor does, fails on the rootfs.
	if err := earlyBoot(linux.LeaveRootfs, linux.MountPseudoFilesystems); err != nil {
		log.Error("boot refused; nothing is started", "err", err)
		refuse(sigs)
	}

	reaper := linux.NewReaper()
	go reaper.Run(context.Background())

	host := linux.NewHost(diskWait, reaper)
	host.Log = log
	extensions := extension.NewManager(extension.NewStore(extension.Dir, extensionProbeDir, extension.MaxBytes, linux.Loop{}), linux.Loop{}, config.ExtensionsRoot, version(), log)
	cgroups := linux.NewCgroupTree(cgroupRoot)
	services := service.NewManager(cgroups, linux.NewLauncher(reaper, con), service.SystemClock{}, log)

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
	var upgrades *upgrade.Upgrader
	publish := func() {
		if err := writeInitStatus(config.InitStatusFile, version(), upgrades); err != nil {
			log.Warn("write the status naru-hostd reports", "file", config.InitStatusFile, "err", err)
		}
	}
	upgrades = upgrade.NewWith(upgrade.Config{Version: version(), Started: started, Log: log, Changed: publish, Extensions: func() ([]upgrade.Entry, []upgrade.Image, error) {
		return storedExtensions(configs, extensions)
	}, Stage: stageExtension(extensions), Unstage: func(sha string) { _ = extensions.Delete(sha) }, Prune: func() { pruneExtensions(configs, extensions, log) }})
	upgrades.Detect()
	publish()
	server := api.New(api.Deps{
		Services:   services,
		Logs:       con,
		Version:    version(),
		BootID:     func() (string, error) { return linux.ReadBootID(linux.BootIDPath) },
		Uptime:     func() time.Duration { return time.Since(started) },
		Reboot:     reboot,
		Upgrades:   upgrades,
		Extensions: extensions,
		// The same path the signer's own --socket is derived from.
		SignerSocket:   config.SignerSocket,
		RevocationFile: config.APIRevocationFile,
		Log:            log,
	})
	metricsCtx, stopMetrics := context.WithCancel(context.Background())
	var engine *apply.Engine
	stopRenewing, booted, refusal := bootAndRenew(bootHost{host, extensions}, issuer, services, configs, server, log)
	if booted != nil {
		engine = newEngine(*booted, configs, services, host, issuer, server)
		server.Attach(engine)
		log.Info("the apply engine is ready", "revision", booted.Revision)
		serveMetrics(metricsCtx, booted.Config.Spec.MetricsAddr(), services, server.Revocations, log)
	}
	handingOff, stopHandoff := context.WithCancel(context.Background())
	if booted != nil && booted.Config.Spec.Join != nil {
		go handOff(handingOff, services, engine, config.JoinConfigFile, reboot, log)
	}
	// A trial boot of an upgrade is judged, and a default boot settles an upgrade left unfinished.
	judging, stopJudging := context.WithCancel(context.Background())
	if upgrades.Slot() == upgrade.SlotTrial {
		reason := ""
		if refusal != nil {
			reason = boot.Reason(refusal)
		}
		go upgrades.Watch(judging, reason, services, reboot)
	} else {
		upgrades.Reconcile()
	}

	sig := <-sigs
	stopJudging()
	stopHandoff()
	stopMetrics()
	// A renewal in flight must not restart a service after it is stopped.
	stopRenewing()
	log.Info("stopping services", "signal", sig.String())
	services.StopAll()
	if err := server.Close(); err != nil {
		log.Error("close the Node API", "err", err)
	}
	// Nothing writes to a disk now: end what a service left outside its cgroup, then undo the
	// mounts, the data disk last.
	if err := cgroups.KillDetached(); err != nil {
		log.Error("kill the detached processes", "err", err)
	}
	host.Settle(log, extensionMounts(extensions, log))
	// The console writes in the background; give it a moment to take the last lines, but never
	// wait on a device that does not.
	flushing, stopFlushing := context.WithTimeout(context.Background(), 2*time.Second)
	_ = con.Flush(flushing)
	stopFlushing()
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

// earlyBoot does the two steps that come before anything else, and refuses at the first that
// fails: init is then in a root half deleted or without /proc, and goes no further.
func earlyBoot(leaveRootfs, mountPseudo func() error) error {
	if err := leaveRootfs(); err != nil {
		return fmt.Errorf("leave the rootfs: %w", err)
	}
	if err := mountPseudo(); err != nil {
		return fmt.Errorf("mount pseudo file systems: %w", err)
	}
	return nil
}

// refuse stays up after a refused early boot, so that the reason stays on the console and the
// machine can still be stopped, and never returns: exiting would panic the kernel.
func refuse(sigs <-chan os.Signal) {
	if <-sigs == syscall.SIGINT {
		_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	} else {
		_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	}
	select {}
}

// ignoreSignals has init ignore the signals it does not act on. Go's default for SIGHUP, SIGQUIT
// and SIGABRT is to exit, and if PID 1 exits the kernel panics.
func ignoreSignals() {
	signal.Ignore(syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGABRT)
}

// storedExtensions returns spec.extensions of the stored configuration and the stored images, which
// an upgrade is checked against (ADR 0055).
func storedExtensions(configs *store.Store, extensions *extension.Manager) ([]upgrade.Entry, []upgrade.Image, error) {
	st, err := configs.Load()
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var entries []upgrade.Entry
	for _, e := range st.Config.Spec.Extensions {
		entries = append(entries, upgrade.Entry{Name: e.Name, SHA256: e.SHA256})
	}
	if len(entries) == 0 {
		return nil, nil, nil
	}
	have, err := extensions.List()
	if err != nil {
		return nil, nil, err
	}
	var images []upgrade.Image
	for _, e := range have {
		images = append(images, upgrade.Image{Name: e.Name, SHA256: e.SHA256, Version: e.Version})
	}
	return entries, images, nil
}

// stageExtension stores an extension member of an upgrade bundle, and says what is wrong with it
// in the terms the upgrade's refusals use (ADR 0084).
func stageExtension(extensions *extension.Manager) func(name, sha256, version string, body io.Reader) (bool, error) {
	return func(name, sha256, version string, body io.Reader) (bool, error) {
		_, added, err := extensions.Stage(body, sha256, name, version)
		switch {
		case errors.Is(err, extension.ErrTooLarge), errors.Is(err, syscall.ENOSPC):
			return added, fmt.Errorf("%w: the extension %s: %v", upgrade.ErrTooLarge, name, err)
		case errors.Is(err, extension.ErrDigest), errors.Is(err, extension.ErrImage), errors.Is(err, extension.ErrStage):
			return added, fmt.Errorf("%w: the extension %s: %v", upgrade.ErrBundle, name, err)
		}
		return added, err
	}
}

// pruneExtensions removes the images built for another version than init's that no pinned entry of
// the configuration names.
func pruneExtensions(configs *store.Store, extensions *extension.Manager, log *slog.Logger) {
	st, err := configs.Load()
	if errors.Is(err, store.ErrNotFound) {
		return // nothing is known to be pinned, so nothing is removed
	}
	if err != nil {
		log.Error("read the configuration to remove the extension images of another version", "err", err)
		return
	}
	if _, err := extensions.Prune(st.Config.Spec.Extensions); err != nil {
		log.Error("remove the extension images of another version", "err", err)
	}
}

// extensionMounts returns where the system extensions are mounted.
func extensionMounts(extensions *extension.Manager, log *slog.Logger) []string {
	entries, err := extensions.List()
	if err != nil {
		log.Error("list the extensions to unmount them", "err", err)
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.Mounted {
			dirs = append(dirs, e.MountPoint)
		}
	}
	return dirs
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
