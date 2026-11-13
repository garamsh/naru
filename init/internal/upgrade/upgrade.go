// Package upgrade carries out the in-place A/B upgrade of a Node (ADR 0025, ADR 0020): it writes
// a bundle's kernel to the trial slot of the ESP and arms the firmware to boot it once, tells a
// trial boot from a default one, promotes a trial that proved healthy by one rename, and cleans up
// after a trial that did not.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/garamsh/naru/init/internal/service"
)

// The two slots a boot can be in.
const (
	SlotDefault = "default"
	SlotTrial   = "trial"
)

// The bounds an upgrade runs within.
const (
	// MaxBundleBytes caps the body of an upgrade: the ESP holds one kernel of the image's size per
	// slot, and this is far above the largest kernel the image builds.
	MaxBundleBytes = 1 << 30
	// Settle is how long every service of a trial boot must have run without a restart for the
	// trial to be healthy.
	Settle = 30 * time.Second
	// Bound is how long after init's start a trial boot has to become healthy; after it, init
	// reboots without promoting.
	Bound = 180 * time.Second
	// poll is how often a trial boot's services are looked at.
	poll = time.Second
)

// Config is where the upgrade reads and writes, and what it runs on. Every field has the value of
// the running machine in New; tests give their own.
type Config struct {
	// Version is the version of the naru-init that runs.
	Version string
	// ESPGUID is the partition GUID of the ESP.
	ESPGUID string
	// SysClassBlock and DevDir are where the block devices are described and named.
	SysClassBlock, DevDir string
	// ESPMount is where the ESP is mounted while it is written, and EFIVars where efivarfs is.
	ESPMount, EFIVars string
	// StateDir is the upgrade directory of the data disk.
	StateDir string
	// MaxBytes caps a bundle.
	MaxBytes int64
	// Settle and Bound are the health bounds of a trial boot, measured from Started.
	Settle, Bound time.Duration
	Started       time.Time
	// Mount and Unmount are syscall.Mount and syscall.Unmount.
	Mount   func(source, target, fstype string, flags uintptr, data string) error
	Unmount func(target string, flags int) error
	// Now and After are the clock and a timer.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time
	Log   *slog.Logger
}

// Upgrader is the upgrade of the Node init runs.
type Upgrader struct {
	cfg Config
	// installing is held while a bundle is received.
	installing sync.Mutex

	mu   sync.Mutex
	slot string
}

// New returns the Upgrader of the running machine for the naru-init of version.
func New(version string, started time.Time, log *slog.Logger) *Upgrader {
	return NewWith(Config{Version: version, Started: started, Log: log})
}

// NewWith returns an Upgrader for cfg, with the running machine's value for each field left empty.
func NewWith(cfg Config) *Upgrader {
	set := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	set(&cfg.ESPGUID, ESPGUID)
	set(&cfg.SysClassBlock, "/sys/class/block")
	set(&cfg.DevDir, "/dev")
	set(&cfg.ESPMount, "/run/naru/esp")
	set(&cfg.EFIVars, "/sys/firmware/efi/efivars")
	set(&cfg.StateDir, "/var/lib/naru/upgrade")
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = MaxBundleBytes
	}
	if cfg.Settle == 0 {
		cfg.Settle = Settle
	}
	if cfg.Bound == 0 {
		cfg.Bound = Bound
	}
	if cfg.Mount == nil {
		cfg.Mount = syscall.Mount
	}
	if cfg.Unmount == nil {
		cfg.Unmount = syscall.Unmount
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Started.IsZero() {
		cfg.Started = cfg.Now()
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Upgrader{cfg: cfg, slot: SlotDefault}
}

// Detect mounts efivarfs and reads which firmware entry booted this system: it is a trial boot
// when BootCurrent is the trial entry, 0x0100. A machine that has no EFI variables is a default
// boot.
func (u *Upgrader) Detect() {
	if err := os.MkdirAll(u.cfg.EFIVars, 0o755); err == nil {
		if err := u.cfg.Mount("efivarfs", u.cfg.EFIVars, "efivarfs", syscall.MS_NODEV|syscall.MS_NOSUID|syscall.MS_NOEXEC, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
			u.cfg.Log.Warn("efivarfs is not mounted; the slot is taken to be the default", "err", err)
		}
	}
	cur, err := readUint16(u.cfg.EFIVars, "BootCurrent")
	u.mu.Lock()
	defer u.mu.Unlock()
	u.slot = SlotDefault
	if err == nil && cur == trialEntry {
		u.slot = SlotTrial
	}
	u.cfg.Log.Info("the boot slot", "slot", u.slot)
}

// Slot is "trial" on a boot that the trial entry made, and "default" otherwise.
func (u *Upgrader) Slot() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.slot
}

// Staged is an upgrade whose trial slot is written and armed.
type Staged struct{ From, To string }

// Install receives the bundle from body into the trial slot and arms the firmware to boot it once.
// wantSHA is the SHA-256 the caller expects of the body. A body that does not have it, or is not a
// bundle, is refused with ErrBundle, one over the cap with ErrTooLarge, and a Node that cannot
// take an upgrade now with ErrBusy; each leaves the default slot and the variables as they were
// and removes what it wrote of the trial slot. Install does not reboot.
func (u *Upgrader) Install(body io.Reader, wantSHA string) (Staged, error) {
	if !u.installing.TryLock() {
		return Staged{}, fmt.Errorf("%w: another upgrade is being received", ErrBusy)
	}
	defer u.installing.Unlock()
	if u.Slot() == SlotTrial {
		return Staged{}, fmt.Errorf("%w: this boot is a trial of an upgrade that has not ended", ErrBusy)
	}
	if _, ok := u.readPending(); ok {
		return Staged{}, fmt.Errorf("%w: an upgrade is staged and has not ended", ErrBusy)
	}
	if !isSHA256(wantSHA) {
		return Staged{}, fmt.Errorf("%w: the expected SHA-256 is not 64 hexadecimal digits", ErrBundle)
	}
	if _, err := os.Stat(u.cfg.EFIVars); err != nil {
		return Staged{}, fmt.Errorf("the Node was not started by UEFI firmware: %w", err)
	}
	p, err := findESP(u.cfg.SysClassBlock, u.cfg.DevDir, u.cfg.ESPGUID)
	if err != nil {
		return Staged{}, fmt.Errorf("find the ESP: %w", err)
	}

	var m Manifest
	err = u.withESP(p, func(dir string) error {
		trial := filepath.Join(dir, trialSlot)
		if err := os.Remove(trial); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		f, err := os.OpenFile(trial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		m, err = receive(body, u.cfg.MaxBytes, wantSHA, f)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(trial)
			_ = syncDir(dir)
			return err
		}
		return syncDir(dir)
	})
	if err != nil {
		return Staged{}, err
	}

	staged := Staged{From: u.cfg.Version, To: m.Version}
	if err := u.arm(p, staged); err != nil {
		u.disarm(p)
		return Staged{}, err
	}
	u.cfg.Log.Info("upgrade staged", "from", staged.From, "to", staged.To, "kernel_bytes", m.Kernel.Bytes, "kernel_sha256", m.Kernel.SHA256)
	return staged, nil
}

// arm records the upgrade, writes the trial entry and has the firmware boot it once.
func (u *Upgrader) arm(p esp, s Staged) error {
	if err := u.writePending(pending{From: s.From, To: s.To}); err != nil {
		return fmt.Errorf("record the upgrade on the data disk: %w", err)
	}
	entry := loadOption("naru trial", p.devicePath(trialFirmware))
	if err := writeVariable(u.cfg.EFIVars, bootName(trialEntry), entry); err != nil {
		return fmt.Errorf("write %s: %w", bootName(trialEntry), err)
	}
	next := []byte{byte(trialEntry & 0xff), byte(trialEntry >> 8)}
	if err := writeVariable(u.cfg.EFIVars, "BootNext", next); err != nil {
		return fmt.Errorf("write BootNext: %w", err)
	}
	return nil
}

// disarm undoes an arm that failed part way, and removes the trial slot.
func (u *Upgrader) disarm(p esp) {
	if _, err := u.dropTrial(p); err != nil {
		u.cfg.Log.Error("remove the trial slot after a failed upgrade", "err", err)
	}
	if err := errors.Join(deleteVariable(u.cfg.EFIVars, bootName(trialEntry)), u.removePending()); err != nil {
		u.cfg.Log.Error("undo a failed upgrade", "err", err)
	}
	if n, err := readUint16(u.cfg.EFIVars, "BootNext"); err == nil && n == trialEntry {
		_ = deleteVariable(u.cfg.EFIVars, "BootNext")
	}
}

// dropTrial deletes TRIAL.EFI from the ESP and reports whether it was there.
func (u *Upgrader) dropTrial(p esp) (was bool, err error) {
	err = u.withESP(p, func(dir string) error {
		err := os.Remove(filepath.Join(dir, trialSlot))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		was = err == nil
		if err != nil {
			return err
		}
		return syncDir(dir)
	})
	return was, err
}

// Reconcile settles an upgrade a default boot finds unfinished: a trial slot or a trial entry
// that this boot did not come from means the trial did not promote itself, so the slot and the
// entry are removed and the upgrade is recorded as rolled back with the reason the trial boot left,
// or as promoted when the new kernel is the default and only its bookkeeping was left. It does
// nothing on a trial boot or when no upgrade is unfinished. It needs the data disk.
func (u *Upgrader) Reconcile() {
	if u.Slot() == SlotTrial {
		return
	}
	_, entryErr := readVariable(u.cfg.EFIVars, bootName(trialEntry))
	pend, havePending := u.readPending()
	if entryErr != nil && !havePending {
		return
	}
	trialWas := false
	if p, err := findESP(u.cfg.SysClassBlock, u.cfg.DevDir, u.cfg.ESPGUID); err != nil {
		u.cfg.Log.Error("find the ESP to remove the trial slot", "err", err)
	} else if trialWas, err = u.dropTrial(p); err != nil {
		u.cfg.Log.Error("remove the trial slot", "err", err)
	}
	if err := deleteVariable(u.cfg.EFIVars, bootName(trialEntry)); err != nil {
		u.cfg.Log.Error("delete the trial entry", "err", err)
	}
	if n, err := readUint16(u.cfg.EFIVars, "BootNext"); err == nil && n == trialEntry {
		_ = deleteVariable(u.cfg.EFIVars, "BootNext")
	}

	out := Outcome{From: pend.From, To: pend.To}
	if out.From == "" {
		out.From = u.cfg.Version
	}
	if out.To == "" {
		out.To = "unknown"
	}
	if havePending && !trialWas && pend.To == u.cfg.Version {
		out.Result = ResultPromoted
		u.cfg.Log.Info("upgrade promoted", "from", out.From, "to", out.To)
	} else {
		out.Result, out.Reason = ResultRolledBack, pend.Reason
		if out.Reason == "" {
			out.Reason = "the trial boot did not reach a healthy state"
		}
		u.cfg.Log.Warn("upgrade rolled back", "from", out.From, "to", out.To, "reason", out.Reason)
	}
	if err := u.finish(out); err != nil {
		u.cfg.Log.Error("record the outcome of the upgrade", "err", err)
	}
}

// Promote makes the trial slot the default: it renames TRIAL.EFI to BOOTX64.EFI in one directory,
// makes the rename durable, deletes the trial entry and records the upgrade as promoted.
func (u *Upgrader) Promote() error {
	p, err := findESP(u.cfg.SysClassBlock, u.cfg.DevDir, u.cfg.ESPGUID)
	if err != nil {
		return fmt.Errorf("find the ESP: %w", err)
	}
	err = u.withESP(p, func(dir string) error {
		if err := os.Rename(filepath.Join(dir, trialSlot), filepath.Join(dir, defaultSlot)); err != nil {
			return err
		}
		return syncDir(dir)
	})
	if err != nil {
		return fmt.Errorf("promote the trial slot: %w", err)
	}
	if err := deleteVariable(u.cfg.EFIVars, bootName(trialEntry)); err != nil {
		u.cfg.Log.Error("delete the trial entry", "err", err)
	}
	u.mu.Lock()
	u.slot = SlotDefault // the running kernel is now the default slot's file
	u.mu.Unlock()
	pend, _ := u.readPending()
	if pend.From == "" {
		pend.From = "unknown"
	}
	u.cfg.Log.Info("upgrade promoted", "from", pend.From, "to", u.cfg.Version)
	if err := u.finish(Outcome{From: pend.From, To: u.cfg.Version, Result: ResultPromoted}); err != nil {
		u.cfg.Log.Error("record the outcome of the upgrade", "err", err)
	}
	return nil
}

// giveUp records why a trial boot is not promoted, for the default boot that follows to report.
func (u *Upgrader) giveUp(reason string) {
	u.cfg.Log.Error("upgrade trial is not healthy; rebooting without promoting", "reason", reason)
	pend, _ := u.readPending()
	pend.Reason = reason
	if pend.To == "" {
		pend.To = u.cfg.Version
	}
	if err := u.writePending(pend); err != nil {
		u.cfg.Log.Error("record why the trial is given up", "err", err)
	}
}

// Services is what a trial boot's health is judged on.
type Services interface {
	Status() []service.Status
}

// Watch judges a trial boot and acts on it, and returns once it has. booted is whether the boot
// sequence finished without a refusal. A refusal gives up at once. Otherwise the trial is healthy
// when every started service has been running, and has not restarted, for Settle; a healthy trial
// is promoted. A trial that is not healthy Bound after init started, or whose promotion fails,
// gives up. To give up is to record the reason and call reboot, which must reboot without
// promoting. Watch ends early when ctx does.
func (u *Upgrader) Watch(ctx context.Context, booted bool, svcs Services, reboot func()) {
	if !booted {
		u.giveUp("the boot sequence refused")
		reboot()
		return
	}
	var prev map[string]int
	since := u.cfg.Now()
	for {
		ok, now := true, map[string]int{}
		for _, s := range svcs.Status() {
			now[s.Name] = s.Restarts
			ok = ok && s.State == service.StateRunning
		}
		if !ok || !sameCounts(prev, now) {
			since = u.cfg.Now()
		}
		prev = now
		if ok && u.cfg.Now().Sub(since) >= u.cfg.Settle {
			if err := u.Promote(); err != nil {
				u.giveUp(err.Error())
				reboot()
			}
			return
		}
		left := u.cfg.Bound - u.cfg.Now().Sub(u.cfg.Started)
		if left <= 0 {
			u.giveUp(fmt.Sprintf("the services were not running steadily within %s of init's start", u.cfg.Bound))
			reboot()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-u.cfg.After(min(poll, left)):
		}
	}
}

func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
