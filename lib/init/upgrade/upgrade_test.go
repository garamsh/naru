package upgrade_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/service"
	"github.com/garamsh/naru/lib/init/upgrade"
)

const efiGUID = "8be4df61-93ca-11d2-aa0d-00e098032b8c"

// clock is a fake time: After advances it by what it was asked to wait, so a Watch runs its whole
// bound at once.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) after(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.t
	return ch
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// node is a Node's disk as the upgrade sees it: a sysfs with the ESP's partition, the ESP's files,
// the EFI variables and the data disk's state directory, all under one temporary directory.
type node struct {
	t       *testing.T
	root    string
	clock   *clock
	logs    *safeBuffer
	mounted bool
	mounts  int
	// flushes are the directories Sync was called on, and unflushed counts the unmounts that came
	// with no flush since the ESP was mounted.
	flushes   []string
	flushed   bool
	unflushed int
}

func newNode(t *testing.T) *node {
	t.Helper()
	n := &node{t: t, root: t.TempDir(), clock: &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, logs: &safeBuffer{}}
	part := filepath.Join(n.root, "sys", "vda1")
	require.NoError(t, os.MkdirAll(part, 0o755))
	// A disk's other partition must not be taken for the ESP.
	other := filepath.Join(n.root, "sys", "vda2")
	require.NoError(t, os.MkdirAll(other, 0o755))
	n.write(filepath.Join(other, "uevent"), "PARTUUID=00000000-0000-0000-0000-000000000002\n")
	n.write(filepath.Join(part, "uevent"), "DEVTYPE=partition\nPARTUUID="+strings.ToUpper(upgrade.ESPGUID)+"\n")
	n.write(filepath.Join(part, "partition"), "1\n")
	n.write(filepath.Join(part, "start"), "2048\n")
	n.write(filepath.Join(part, "size"), "262144\n")
	require.NoError(t, os.MkdirAll(n.slots(), 0o755))
	n.write(filepath.Join(n.slots(), "BOOTX64.EFI"), "kernel-A")
	require.NoError(t, os.MkdirAll(n.efivars(), 0o755))
	return n
}

func (n *node) write(path, content string) {
	n.t.Helper()
	require.NoError(n.t, os.WriteFile(path, []byte(content), 0o644))
}

func (n *node) slots() string   { return filepath.Join(n.root, "esp", "EFI", "BOOT") }
func (n *node) efivars() string { return filepath.Join(n.root, "efivars") }
func (n *node) state() string   { return filepath.Join(n.root, "state") }

// upgrader returns the Upgrader of the naru-init of version, started when the clock now is.
func (n *node) upgrader(version string, mut ...func(*upgrade.Config)) *upgrade.Upgrader {
	cfg := upgrade.Config{
		Version:       version,
		SysClassBlock: filepath.Join(n.root, "sys"),
		DevDir:        filepath.Join(n.root, "dev"),
		ESPMount:      filepath.Join(n.root, "esp"),
		EFIVars:       n.efivars(),
		StateDir:      n.state(),
		Started:       n.clock.now(),
		Now:           n.clock.now,
		After:         n.clock.after,
		Log:           slog.New(slog.NewTextHandler(n.logs, nil)),
		Mount: func(source, target, fstype string, _ uintptr, _ string) error {
			if fstype == "vfat" {
				require.False(n.t, n.mounted, "the ESP is mounted once at a time")
				n.mounted = true
				n.flushed = false
				n.mounts++
			}
			return nil
		},
		Unmount: func(string, int) error {
			if !n.flushed {
				n.unflushed++
			}
			n.mounted = false
			return nil
		},
		Sync: func(dir string) error {
			n.flushes = append(n.flushes, dir)
			n.flushed = true
			return nil
		},
	}
	for _, m := range mut {
		m(&cfg)
	}
	return upgrade.NewWith(cfg)
}

func (n *node) variable(name string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(n.efivars(), name+"-"+efiGUID))
	if err != nil {
		return nil, false
	}
	return b[4:], true
}

func (n *node) setVariable(name string, value []byte) {
	n.t.Helper()
	b := binary.LittleEndian.AppendUint32(nil, 7)
	require.NoError(n.t, os.WriteFile(filepath.Join(n.efivars(), name+"-"+efiGUID), append(b, value...), 0o644))
}

func (n *node) slot(name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(n.slots(), name))
	return string(b), err == nil
}

// bootedFromTrial is what a reboot into the trial entry leaves for the next init.
func (n *node) bootedFromTrial() { n.setVariable("BootCurrent", []byte{0x00, 0x01}) }

// untouched asserts that the default slot is as it was and no trial slot or variable exists.
func (n *node) untouched() {
	n.t.Helper()
	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(n.t, "kernel-A", a, "the default slot")
	_, trial := n.slot("TRIAL.EFI")
	assert.False(n.t, trial, "the trial slot")
	_, entry := n.variable("Boot0100")
	assert.False(n.t, entry, "Boot0100")
	_, next := n.variable("BootNext")
	assert.False(n.t, next, "BootNext")
	_, err := os.Stat(filepath.Join(n.state(), "pending.json"))
	assert.ErrorIs(n.t, err, os.ErrNotExist, "the record of a staged upgrade")
	assert.False(n.t, n.mounted, "the ESP is unmounted")
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type manifest struct {
	Version string `json:"version"`
	Kernel  struct {
		Name   string `json:"name"`
		Bytes  int    `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"kernel"`
}

// bundle returns an upgrade bundle of the kernel and the manifest mutated by mut, and its SHA-256.
func bundle(t *testing.T, version, kernel string, mut ...func(*manifest)) ([]byte, string) {
	t.Helper()
	m := manifest{Version: version}
	m.Kernel.Name, m.Kernel.Bytes, m.Kernel.SHA256 = "kernel.efi", len(kernel), digest([]byte(kernel))
	for _, f := range mut {
		f(&m)
	}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{{"kernel.efi", kernel}, {"manifest.json", string(raw)}} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body))}))
		_, err := tw.Write([]byte(f.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes(), digest(buf.Bytes())
}

func TestUpgrader_InstallStagesTheKernelInTheTrialSlotAndArmsTheFirmwareToBootItOnce(t *testing.T) {
	n := newNode(t)
	u := n.upgrader("v1")
	b, sum := bundle(t, "v2", "kernel-B")

	staged, err := u.Install(bytes.NewReader(b), sum)

	require.NoError(t, err)
	assert.Equal(t, upgrade.Staged{From: "v1", To: "v2"}, staged)
	trial, ok := n.slot("TRIAL.EFI")
	assert.True(t, ok)
	assert.Equal(t, "kernel-B", trial)
	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-A", a, "the default slot is not touched")
	next, ok := n.variable("BootNext")
	assert.True(t, ok)
	assert.Equal(t, []byte{0x00, 0x01}, next, "BootNext is 0x0100")
	entry, ok := n.variable("Boot0100")
	require.True(t, ok)
	assert.Contains(t, string(entry), "T\x00R\x00I\x00A\x00L\x00.\x00E\x00F\x00I\x00", "the entry boots the trial slot")
	assert.Equal(t, uint32(1), binary.LittleEndian.Uint32(entry), "the entry is active")
	guid := []byte{0xe7, 0x62, 0x41, 0xcb, 0x1e, 0x9d, 0xdf, 0x48, 0xb9, 0x37, 0xf5, 0x79, 0x20, 0x71, 0xb9, 0x50}
	assert.Contains(t, string(entry), string(guid), "the entry names the partition by its GUID")
	assert.False(t, n.mounted, "the ESP is unmounted when Install returns")
	assert.Equal(t, 1, n.mounts)
	_, ok = u.Last()
	assert.False(t, ok, "staging is not an outcome")
}

func TestUpgrader_InstallFlushesTheESPOnceBeforeItUnmountsIt(t *testing.T) {
	n := newNode(t)
	u := n.upgrader("v1")
	b, sum := bundle(t, "v2", "kernel-B")

	_, err := u.Install(bytes.NewReader(b), sum)

	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(n.root, "esp")}, n.flushes, "one flush, of the ESP's mount point")
	assert.Equal(t, 0, n.unflushed, "the ESP was flushed before it was unmounted")
}

func TestUpgrader_InstallReportsAFailedFlushOfTheESPAndStillUnmountsIt(t *testing.T) {
	n := newNode(t)
	boom := errors.New("boom")
	u := n.upgrader("v1", func(c *upgrade.Config) {
		c.Sync = func(string) error { return boom }
	})
	b, sum := bundle(t, "v2", "kernel-B")

	_, err := u.Install(bytes.NewReader(b), sum)

	require.ErrorIs(t, err, boom)
	assert.False(t, n.mounted, "the ESP is unmounted after a failed flush")
}

func TestUpgrader_InstallRefusesABodyThatIsNotTheBundleItWasToldAboutAndTouchesNothing(t *testing.T) {
	good, goodSum := bundle(t, "v2", "kernel-B")
	wrongKernel, wrongKernelSum := bundle(t, "v2", "kernel-B", func(m *manifest) { m.Kernel.SHA256 = digest([]byte("other")) })
	wrongSize, wrongSizeSum := bundle(t, "v2", "kernel-B", func(m *manifest) { m.Kernel.Bytes++ })
	noVersion, noVersionSum := bundle(t, "", "kernel-B")
	trailing := append(append([]byte{}, good...), []byte("trailing")...)
	tests := map[string]struct {
		body     []byte
		sum      string
		maxBytes int64
		wantErr  error
	}{
		"a digest that is not the body's":        {good, digest([]byte("x")), 0, upgrade.ErrBundle},
		"a digest that is not a digest":          {good, "abc", 0, upgrade.ErrBundle},
		"bytes after the bundle":                 {trailing, goodSum, 0, upgrade.ErrBundle},
		"a body that is not a tar":               {[]byte(strings.Repeat("not a tar ", 100)), digest([]byte(strings.Repeat("not a tar ", 100))), 0, upgrade.ErrBundle},
		"an empty body":                          {nil, digest(nil), 0, upgrade.ErrBundle},
		"a manifest of another kernel":           {wrongKernel, wrongKernelSum, 0, upgrade.ErrBundle},
		"a manifest with the wrong size":         {wrongSize, wrongSizeSum, 0, upgrade.ErrBundle},
		"a manifest without a version":           {noVersion, noVersionSum, 0, upgrade.ErrBundle},
		"a bundle over the cap":                  {good, goodSum, 600, upgrade.ErrTooLarge},
		"a bundle truncated before its manifest": {good[:600], digest(good[:600]), 0, upgrade.ErrBundle},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			n := newNode(t)
			if !strings.Contains(name, "not a digest") {
				// A trial slot an earlier upgrade left is removed, not kept. A refusal before the ESP is mounted leaves it.
				n.write(filepath.Join(n.slots(), "TRIAL.EFI"), "stale")
			}
			u := n.upgrader("v1", func(c *upgrade.Config) {
				if tt.maxBytes > 0 {
					c.MaxBytes = tt.maxBytes
				}
			})

			_, err := u.Install(bytes.NewReader(tt.body), tt.sum)

			require.ErrorIs(t, err, tt.wantErr)
			n.untouched()
		})
	}
	t.Run("the control: the bundle itself is staged", func(t *testing.T) {
		n := newNode(t)
		n.write(filepath.Join(n.slots(), "TRIAL.EFI"), "stale")

		_, err := n.upgrader("v1").Install(bytes.NewReader(good), goodSum)

		require.NoError(t, err)
		trial, _ := n.slot("TRIAL.EFI")
		assert.Equal(t, "kernel-B", trial)
	})
}

func TestUpgrader_InstallAcceptsTheDigestInEitherCase(t *testing.T) {
	n := newNode(t)
	b, sum := bundle(t, "v2", "kernel-B")

	_, err := n.upgrader("v1").Install(bytes.NewReader(b), strings.ToUpper(sum))

	require.NoError(t, err)
}

func TestUpgrader_InstallIsRefusedWhereAnUpgradeCannotStart(t *testing.T) {
	b, sum := bundle(t, "v2", "kernel-B")
	t.Run("on a trial boot", func(t *testing.T) {
		n := newNode(t)
		n.bootedFromTrial()
		u := n.upgrader("v2")
		u.Detect()

		_, err := u.Install(bytes.NewReader(b), sum)

		require.ErrorIs(t, err, upgrade.ErrBusy)
		assert.Equal(t, 0, n.mounts)
	})
	t.Run("while another is staged", func(t *testing.T) {
		n := newNode(t)
		u := n.upgrader("v1")
		_, err := u.Install(bytes.NewReader(b), sum)
		require.NoError(t, err, "the control: the first is staged")

		_, err = u.Install(bytes.NewReader(b), sum)

		require.ErrorIs(t, err, upgrade.ErrBusy)
	})
	t.Run("on a machine that has no EFI variables", func(t *testing.T) {
		n := newNode(t)
		u := n.upgrader("v1", func(c *upgrade.Config) { c.EFIVars = filepath.Join(n.root, "none") })

		_, err := u.Install(bytes.NewReader(b), sum)

		require.Error(t, err)
		assert.NotErrorIs(t, err, upgrade.ErrBundle)
		assert.Equal(t, 0, n.mounts)
	})
	t.Run("on a disk without the ESP", func(t *testing.T) {
		n := newNode(t)
		u := n.upgrader("v1", func(c *upgrade.Config) { c.ESPGUID = "00000000-0000-0000-0000-0000000000ff" })

		_, err := u.Install(bytes.NewReader(b), sum)

		require.Error(t, err)
		assert.Equal(t, 0, n.mounts)
	})
}

func TestUpgrader_DetectTakesTheBootForATrialOnlyWhenBootCurrentIs0x0100(t *testing.T) {
	tests := map[string]struct {
		current []byte
		want    string
	}{
		"the trial entry":           {[]byte{0x00, 0x01}, upgrade.SlotTrial},
		"the default entry":         {[]byte{0x01, 0x00}, upgrade.SlotDefault},
		"another entry":             {[]byte{0x00, 0x02}, upgrade.SlotDefault},
		"a value of the wrong size": {[]byte{0x00, 0x01, 0x00}, upgrade.SlotDefault},
		"no BootCurrent":            {nil, upgrade.SlotDefault},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			n := newNode(t)
			if tt.current != nil {
				n.setVariable("BootCurrent", tt.current)
			}
			u := n.upgrader("v1")
			assert.Equal(t, upgrade.SlotDefault, u.Slot(), "a default boot until it is detected")

			u.Detect()

			assert.Equal(t, tt.want, u.Slot())
		})
	}
}

// stage stages an upgrade from v1 to v2 and returns the Upgrader of the trial boot that follows.
func stage(t *testing.T, n *node) *upgrade.Upgrader {
	t.Helper()
	b, sum := bundle(t, "v2", "kernel-B")
	_, err := n.upgrader("v1").Install(bytes.NewReader(b), sum)
	require.NoError(t, err)
	n.bootedFromTrial()
	u := n.upgrader("v2")
	u.Detect()
	require.Equal(t, upgrade.SlotTrial, u.Slot())
	n.mounts = 0
	return u
}

func TestUpgrader_PromoteRenamesTheTrialSlotOverTheDefaultAndDeletesTheEntry(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)

	require.NoError(t, u.Promote())

	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-B", a, "the default slot holds the new kernel")
	_, trial := n.slot("TRIAL.EFI")
	assert.False(t, trial, "the trial slot is gone: it was renamed, not copied")
	_, entry := n.variable("Boot0100")
	assert.False(t, entry)
	last, ok := u.Last()
	require.True(t, ok)
	assert.Equal(t, "promoted", last.Result)
	assert.Equal(t, "v1", last.From)
	assert.Equal(t, "v2", last.To)
	assert.Equal(t, "2026-01-01T00:00:00Z", last.Time)
	assert.Equal(t, upgrade.SlotDefault, u.Slot(), "the running kernel is the default slot's file now")
	assert.Contains(t, n.logs.String(), "upgrade promoted")
	_, err := os.Stat(filepath.Join(n.state(), "pending.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	assert.False(t, n.mounted)
}

func TestUpgrader_PromoteWithoutATrialSlotFailsAndLeavesTheDefaultSlot(t *testing.T) {
	n := newNode(t)
	u := n.upgrader("v2")

	require.Error(t, u.Promote())

	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-A", a)
	_, ok := u.Last()
	assert.False(t, ok)
	assert.False(t, n.mounted)
}

func TestUpgrader_ReconcileOnADefaultBootAfterATrialThatDidNotPromoteRollsBack(t *testing.T) {
	n := newNode(t)
	stage(t, n)
	// The trial boot is gone, and the next boot is the default's.
	require.NoError(t, os.Remove(filepath.Join(n.efivars(), "BootCurrent-"+efiGUID)))
	n.setVariable("BootCurrent", []byte{0x01, 0x00})
	u := n.upgrader("v1")
	u.Detect()

	u.Reconcile()

	_, trial := n.slot("TRIAL.EFI")
	assert.False(t, trial, "the trial slot is removed")
	_, entry := n.variable("Boot0100")
	assert.False(t, entry, "the trial entry is deleted")
	_, next := n.variable("BootNext")
	assert.False(t, next)
	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-A", a)
	last, ok := u.Last()
	require.True(t, ok)
	assert.Equal(t, upgrade.Outcome{From: "v1", To: "v2", Result: "rolled back", Reason: last.Reason, Time: "2026-01-01T00:00:00Z"}, last)
	assert.NotEmpty(t, last.Reason)
	assert.Contains(t, n.logs.String(), "upgrade rolled back")
	assert.False(t, n.mounted)
}

func TestUpgrader_ReconcileReportsTheReasonTheTrialBootGaveUp(t *testing.T) {
	n := newNode(t)
	trial := stage(t, n)
	// The trial boot gave up, rebooted, and the firmware came back to the default.
	trial.Watch(context.Background(), false, &counting{script: func(int) []service.Status { return nil }}, func() {})
	require.NoError(t, os.Remove(filepath.Join(n.efivars(), "BootCurrent-"+efiGUID)))
	u := n.upgrader("v1")
	u.Detect()

	u.Reconcile()

	last, ok := u.Last()
	require.True(t, ok)
	assert.Equal(t, "rolled back", last.Result)
	assert.Equal(t, "the boot sequence refused", last.Reason)
	assert.Contains(t, n.logs.String(), "reason=\"the boot sequence refused\"")
}

func TestUpgrader_ReconcileDoesNothingWhenNoUpgradeIsUnfinishedAndDoesNotMountTheESP(t *testing.T) {
	n := newNode(t)
	u := n.upgrader("v1")
	u.Detect()

	u.Reconcile()

	assert.Equal(t, 0, n.mounts, "an ordinary boot does not mount the ESP")
	_, ok := u.Last()
	assert.False(t, ok)
	assert.NotContains(t, n.logs.String(), "upgrade rolled back")
}

func TestUpgrader_ReconcileLeavesATrialBootAlone(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)

	u.Reconcile()

	_, trial := n.slot("TRIAL.EFI")
	assert.True(t, trial, "the trial slot is the one running")
	_, entry := n.variable("Boot0100")
	assert.True(t, entry)
	_, ok := u.Last()
	assert.False(t, ok)
}

func TestUpgrader_ReconcileRecordsPromotedWhenOnlyTheBookkeepingWasLeft(t *testing.T) {
	n := newNode(t)
	stage(t, n)
	// The rename was made and the trial boot went down before its bookkeeping.
	require.NoError(t, os.Rename(filepath.Join(n.slots(), "TRIAL.EFI"), filepath.Join(n.slots(), "BOOTX64.EFI")))
	require.NoError(t, os.Remove(filepath.Join(n.efivars(), "BootCurrent-"+efiGUID)))
	d := n.upgrader("v2")
	d.Detect()

	d.Reconcile()

	last, ok := d.Last()
	require.True(t, ok)
	assert.Equal(t, "promoted", last.Result)
	_, entry := n.variable("Boot0100")
	assert.False(t, entry)
}

// counting reports the statuses that script returns for the number of the call.
type counting struct {
	calls  int
	script func(call int) []service.Status
}

func (c *counting) Status() []service.Status {
	c.calls++
	return c.script(c.calls)
}

func running(restarts ...int) []service.Status {
	var out []service.Status
	for i, r := range restarts {
		out = append(out, service.Status{Name: string(rune('a' + i)), State: service.StateRunning, Restarts: r})
	}
	return out
}

func TestUpgrader_WatchPromotesATrialWhoseServicesRanSteadyForTheSettleTime(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)
	rebooted := false

	u.Watch(context.Background(), true, &counting{script: func(int) []service.Status { return running(0, 0) }}, func() { rebooted = true })

	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-B", a)
	assert.False(t, rebooted, "a healthy trial is not rebooted")
	assert.GreaterOrEqual(t, n.clock.now().Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), upgrade.Settle)
	assert.Contains(t, n.logs.String(), "upgrade promoted")
}

func TestUpgrader_WatchPromotesATrialThatStartedNoService(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)

	u.Watch(context.Background(), true, &counting{script: func(int) []service.Status { return nil }}, func() { t.Fatal("rebooted") })

	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-B", a)
}

func TestUpgrader_WatchDoesNotPromoteBeforeTheSettleTimeHasPassedSinceTheLastRestart(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)
	start := n.clock.now()
	var promotedAt time.Duration
	svcs := &counting{script: func(call int) []service.Status {
		if _, ok := n.slot("TRIAL.EFI"); !ok && promotedAt == 0 {
			promotedAt = n.clock.now().Sub(start)
		}
		// One service restarts at the 20th look.
		if call >= 20 {
			return running(0, 1)
		}
		return running(0, 0)
	}}

	u.Watch(context.Background(), true, svcs, func() { t.Fatal("rebooted") })

	a, _ := n.slot("BOOTX64.EFI")
	require.Equal(t, "kernel-B", a, "the control: the trial is promoted in the end")
	assert.GreaterOrEqual(t, n.clock.now().Sub(start), 19*time.Second+upgrade.Settle, "the settle time runs from the restart")
}

func TestUpgrader_WatchGivesUpAtOnceOnABootThatRefused(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)
	rebooted := 0

	u.Watch(context.Background(), false, &counting{script: func(int) []service.Status { return running(0) }}, func() { rebooted++ })

	assert.Equal(t, 1, rebooted)
	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-A", a, "nothing is promoted")
	_, trial := n.slot("TRIAL.EFI")
	assert.True(t, trial, "the trial slot is left to the default boot that follows")
	_, entry := n.variable("Boot0100")
	assert.True(t, entry)
	assert.Contains(t, n.logs.String(), "the boot sequence refused")
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), n.clock.now(), "no waiting")
}

func TestUpgrader_WatchGivesUpWhenAServiceIsNotRunningWithinTheBound(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)
	rebooted := 0
	svcs := &counting{script: func(int) []service.Status {
		return []service.Status{{Name: "a", State: service.StateRunning}, {Name: "b", State: service.StateBackoff}}
	}}

	u.Watch(context.Background(), true, svcs, func() { rebooted++ })

	assert.Equal(t, 1, rebooted)
	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-A", a)
	assert.GreaterOrEqual(t, n.clock.now().Sub(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), upgrade.Bound)
	assert.Contains(t, n.logs.String(), "upgrade trial is not healthy")
}

func TestUpgrader_WatchGivesUpWhenAServiceKeepsRestarting(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)
	rebooted := 0
	svcs := &counting{script: func(call int) []service.Status { return running(call) }}

	u.Watch(context.Background(), true, svcs, func() { rebooted++ })

	assert.Equal(t, 1, rebooted)
	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-A", a)
}

func TestUpgrader_WatchEndsWhenItsContextDoes(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svcs := &counting{script: func(int) []service.Status {
		return []service.Status{{Name: "a", State: service.StateBackoff}}
	}}

	u.Watch(ctx, true, svcs, func() { t.Fatal("rebooted") })

	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-A", a)
}

func TestUpgrader_WatchGivesUpWhenThePromotionFails(t *testing.T) {
	n := newNode(t)
	u := stage(t, n)
	require.NoError(t, os.Remove(filepath.Join(n.slots(), "TRIAL.EFI")))
	rebooted := 0

	u.Watch(context.Background(), true, &counting{script: func(int) []service.Status { return running(0) }}, func() { rebooted++ })

	assert.Equal(t, 1, rebooted)
	a, _ := n.slot("BOOTX64.EFI")
	assert.Equal(t, "kernel-A", a)
}
