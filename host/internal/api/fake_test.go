package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
)

// fakeRuntime is an in-memory mvmd.
type fakeRuntime struct {
	mu       sync.Mutex
	machines map[string]mvmd.Machine
	disks    map[string]mvmd.Disk
	pulled   []string
	info     mvmd.Info
	err      error
	// calls are the calls that change mvmd, in order; failAt maps one of them to the error it
	// returns instead of doing its work; contents are the contents each raw disk was made with.
	calls    []string
	failAt   map[string]error
	contents map[string][]byte
	// ignoreRequested makes CreateMachine give its own address and not the requested one, as an mvmd
	// that predates mvmd#817 does.
	ignoreRequested bool
	// stops counts the StopMachine calls that reached mvmd.
	stops int
	// undoErrs are the context errors the StopMachine and DeleteMachine calls found.
	undoErrs []error
	// infoHook, when set, runs first in Info: a test blocks it, or fails it with the error mvmd gave.
	infoHook func(context.Context) error
	// fences is the state of each fenced Machine's own fence; snapshots are the snapshots held.
	fences    map[string]mvmd.Deadline
	snapshots map[string]mvmd.Snapshot
	// bootSets are the boot sets added, and bootFiles the kernel and initrd each was given.
	bootSets  map[string]mvmd.BootSet
	bootFiles map[string][2][]byte
	// created is the request each Machine was created with; graces the grace of each stop;
	// diskSnapshots the disk snapshots held, and diskFrom the snapshot each disk was made from.
	created       map[string]mvmd.Create
	graces        []time.Duration
	diskSnapshots map[string]mvmd.DiskSnapshot
	diskFrom      map[string]string
	// guest is each Machine's guest files, and execs the execs started, by ID.
	guest map[string]map[string][]byte
	execs map[string]*fakeExec
	// data is each disk's bytes from offset 0, zero past them; filesystems and digests are what an
	// import made a disk with.
	data        map[string][]byte
	filesystems map[string]string
	digests     map[string]string
	// corrupt makes ReadDisk send other bytes than a disk's under its true digest.
	corrupt map[string]bool
	// stallAt makes ReadDisk send a disk's parts before the index and then nothing until its call
	// ends; truncate makes it end a disk's stream without its digest.
	stallAt  map[string]int
	truncate map[string]bool
}

// fakeExec is an exec the fake runs: its output and record, and done, closed once it ended.
type fakeExec struct {
	command []string
	output  []mvmd.Chunk
	record  mvmd.ExecRecord
	done    chan struct{}
	signals []int32
}

// step records a call that changes mvmd and returns the error a test set for it.
func (f *fakeRuntime) step(name string) error {
	f.calls = append(f.calls, name)
	return f.failAt[name]
}

func newFake() *fakeRuntime {
	return &fakeRuntime{
		machines: map[string]mvmd.Machine{}, disks: map[string]mvmd.Disk{}, failAt: map[string]error{}, contents: map[string][]byte{},
		fences: map[string]mvmd.Deadline{}, snapshots: map[string]mvmd.Snapshot{},
		bootSets: map[string]mvmd.BootSet{}, bootFiles: map[string][2][]byte{},
		created: map[string]mvmd.Create{}, diskSnapshots: map[string]mvmd.DiskSnapshot{}, diskFrom: map[string]string{},
		guest: map[string]map[string][]byte{}, execs: map[string]*fakeExec{},
		data: map[string][]byte{}, filesystems: map[string]string{}, digests: map[string]string{},
		info: mvmd.Info{
			APIVersion: "v1alpha2", Driver: "fake", CPUs: 8, MemoryBytes: 1 << 33, StateTotalBytes: 100, StateAvailableBytes: 60,
			MachinePrefixes: []string{"10.1.0.0/24"},
		},
	}
}

func (f *fakeRuntime) Info(ctx context.Context) (mvmd.Info, error) {
	if f.infoHook != nil {
		if err := f.infoHook(ctx); err != nil {
			return mvmd.Info{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// Like mvmd, the boot sets offered are the configured ones and the ones added, by name.
	info := f.info
	info.BootSets = slices.Clone(f.info.BootSets)
	for _, b := range f.bootSets {
		b.Origin = "added"
		info.BootSets = append(info.BootSets, b)
	}
	sort.Slice(info.BootSets, func(i, j int) bool { return info.BootSets[i].Name < info.BootSets[j].Name })
	return info, f.err
}

func (f *fakeRuntime) Pull(_ context.Context, repo, digest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled = append(f.pulled, repo+"@"+digest)
	return f.err
}

func (f *fakeRuntime) CreateDisk(_ context.Context, id string, size uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("createDisk " + id); err != nil {
		return err
	}
	if _, ok := f.disks[id]; ok {
		return mvmd.ErrConflict
	}
	f.disks[id] = mvmd.Disk{ID: id, SizeBytes: size}
	return f.err
}

// CreateDiskFrom is mvmd's: an unknown snapshot is not found, a size below the snapshot's invalid.
func (f *fakeRuntime) CreateDiskFrom(_ context.Context, id string, size uint64, snapshot string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("createDiskFrom " + id + " " + snapshot); err != nil {
		return err
	}
	src, ok := f.diskSnapshots[snapshot]
	if !ok {
		return fmt.Errorf("disk snapshot %s: %w", snapshot, mvmd.ErrNotFound)
	}
	if size < src.SizeBytes {
		return fmt.Errorf("%w: %d is below the snapshot's %d", mvmd.ErrInvalid, size, src.SizeBytes)
	}
	if _, ok := f.disks[id]; ok {
		return mvmd.ErrConflict
	}
	f.disks[id] = mvmd.Disk{ID: id, SizeBytes: size}
	f.diskFrom[id] = snapshot
	return f.err
}

// SnapshotDisk is mvmd's: the same ID of the same disk is the snapshot taken, of another disk a
// conflict.
func (f *fakeRuntime) SnapshotDisk(_ context.Context, snapshot, disk string) (mvmd.DiskSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("snapshotDisk " + snapshot + " " + disk); err != nil {
		return mvmd.DiskSnapshot{}, err
	}
	if have, ok := f.diskSnapshots[snapshot]; ok {
		if have.DiskID != disk {
			return mvmd.DiskSnapshot{}, fmt.Errorf("%w: the disk snapshot %s is of %s", mvmd.ErrConflict, snapshot, have.DiskID)
		}
		return have, nil
	}
	d, ok := f.disks[disk]
	if !ok {
		return mvmd.DiskSnapshot{}, mvmd.ErrNotFound
	}
	sn := mvmd.DiskSnapshot{ID: snapshot, DiskID: disk, SizeBytes: d.SizeBytes, AllocatedBytes: d.SizeBytes / 2, CreatedAt: time.Unix(1764000000, 0).UTC()}
	f.diskSnapshots[snapshot] = sn
	return sn, nil
}

func (f *fakeRuntime) ListDiskSnapshots(context.Context) ([]mvmd.DiskSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mvmd.DiskSnapshot
	for _, d := range f.diskSnapshots {
		out = append(out, d)
	}
	return out, f.err
}

func (f *fakeRuntime) DeleteDiskSnapshot(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.step("deleteDiskSnapshot " + id)
	if _, ok := f.diskSnapshots[id]; !ok {
		return mvmd.ErrNotFound
	}
	delete(f.diskSnapshots, id)
	return nil
}

func (f *fakeRuntime) CreateRawDisk(_ context.Context, id string, size uint64, contents []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("createRawDisk " + id); err != nil {
		return err
	}
	if _, ok := f.disks[id]; ok {
		return mvmd.ErrConflict
	}
	if size == 0 {
		size = (uint64(len(contents)) + 511) / 512 * 512
	}
	f.disks[id] = mvmd.Disk{ID: id, SizeBytes: size}
	f.contents[id] = contents
	return f.err
}

func (f *fakeRuntime) AttachDisk(_ context.Context, machine string, a mvmd.Attachment) (mvmd.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("attachDisk " + a.ID); err != nil {
		return mvmd.Machine{}, err
	}
	m, ok := f.machines[machine]
	if !ok {
		return mvmd.Machine{}, mvmd.ErrNotFound
	}
	if m.State != mvmd.StateCreated && m.State != mvmd.StateStopped {
		return mvmd.Machine{}, mvmd.ErrPrecondition
	}
	m.Disks = append(slices.Clone(m.Disks), a)
	f.machines[machine] = m
	return m, nil
}

func (f *fakeRuntime) DeleteDisk(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = f.step("deleteDisk " + id)
	if _, ok := f.disks[id]; !ok {
		return mvmd.ErrNotFound
	}
	delete(f.disks, id)
	return nil
}

func (f *fakeRuntime) ListDisks(context.Context) ([]mvmd.Disk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mvmd.Disk
	for _, d := range f.disks {
		out = append(out, d)
	}
	return out, f.err
}

func (f *fakeRuntime) CreateMachine(_ context.Context, c mvmd.Create) (mvmd.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("createMachine " + c.ID); err != nil {
		return mvmd.Machine{}, err
	}
	if f.err != nil {
		return mvmd.Machine{}, f.err
	}
	m := mvmd.Machine{
		BootSet: c.BootSet, Cmdline: c.Cmdline, RoutedPrefixes: c.RoutedPrefixes, RestartOnBoot: c.RestartOnBoot, Fenced: c.Fenced,
		ID: c.ID, State: mvmd.StateCreated, ImageDigest: c.ImageDigest, CPU: c.CPU, MemoryBytes: c.MemoryBytes,
		RootDiskBytes: c.RootDiskBytes, Disks: c.Disks, Policy: c.Policy, Addresses: []string{"10.1.0.5"},
		WorkingDir: c.WorkingDir,
	}
	// Like mvmd, the environment by name and the files without their content.
	for name := range c.Env {
		m.EnvNames = append(m.EnvNames, name)
	}
	sort.Strings(m.EnvNames)
	for _, file := range c.Files {
		m.Files = append(m.Files, mvmd.FileInfo{Path: file.Path, Mode: file.Mode, SizeBytes: uint64(len(file.Content))})
	}
	if c.RequestedIPv4 != "" {
		// Like mvmd: an address outside the node's Machine prefix is invalid, one another Machine holds a conflict.
		a, _ := netip.ParseAddr(c.RequestedIPv4)
		if !netip.MustParsePrefix(f.info.MachinePrefixes[0]).Contains(a) {
			return mvmd.Machine{}, fmt.Errorf("%w: %s is outside the Machine prefix", mvmd.ErrInvalid, c.RequestedIPv4)
		}
		for _, other := range f.machines {
			if slices.Contains(other.Addresses, c.RequestedIPv4) {
				return mvmd.Machine{}, fmt.Errorf("%w: %s is held by %s", mvmd.ErrConflict, c.RequestedIPv4, other.ID)
			}
		}
		if !f.ignoreRequested {
			m.Addresses = []string{c.RequestedIPv4}
		}
	}
	f.machines[c.ID] = m
	f.created[c.ID] = c
	return m, nil
}

func (f *fakeRuntime) StartMachine(_ context.Context, id string) (mvmd.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("start " + id); err != nil {
		return mvmd.Machine{}, err
	}
	m, ok := f.machines[id]
	if !ok {
		return mvmd.Machine{}, mvmd.ErrNotFound
	}
	if m.Fenced && f.fences[id].Phase != "armed" {
		return mvmd.Machine{}, mvmd.ErrPrecondition
	}
	m.State = mvmd.StateRunning
	f.machines[id] = m
	return m, nil
}

func (f *fakeRuntime) StopMachine(ctx context.Context, id string, grace time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	f.graces = append(f.graces, grace)
	f.undoErrs = append(f.undoErrs, ctx.Err())
	m, ok := f.machines[id]
	if !ok {
		return mvmd.ErrNotFound
	}
	m.State = mvmd.StateStopped
	f.machines[id] = m
	return nil
}

func (f *fakeRuntime) DeleteMachine(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.undoErrs = append(f.undoErrs, ctx.Err())
	_ = f.step("deleteMachine " + id)
	if m, ok := f.machines[id]; ok && m.State == mvmd.StateRunning {
		return mvmd.ErrPrecondition
	}
	delete(f.machines, id)
	return nil
}

func (f *fakeRuntime) InspectMachine(_ context.Context, id string) (mvmd.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.machines[id]
	if !ok {
		return mvmd.Machine{}, mvmd.ErrNotFound
	}
	return m, nil
}

func (f *fakeRuntime) ListMachines(context.Context) ([]mvmd.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mvmd.Machine
	for _, m := range f.machines {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, f.err
}

func (f *fakeRuntime) Renew(context.Context, time.Duration, uint64) error { return nil }

func (f *fakeRuntime) InspectDeadline(context.Context) (mvmd.Deadline, error) {
	return mvmd.Deadline{}, nil
}

// fenceOf returns a fenced Machine's fence, or the error mvmd answers for another Machine.
func (f *fakeRuntime) fenceOf(id string) (mvmd.Deadline, error) {
	m, ok := f.machines[id]
	if !ok {
		return mvmd.Deadline{}, mvmd.ErrNotFound
	}
	if !m.Fenced {
		return mvmd.Deadline{}, mvmd.ErrPrecondition
	}
	d := f.fences[id]
	if d.Phase == "" {
		d.Phase = "notArmed"
	}
	return d, nil
}

func (f *fakeRuntime) RenewMachine(_ context.Context, id string, length time.Duration, number uint64) (mvmd.Deadline, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("renew " + id); err != nil {
		return mvmd.Deadline{}, err
	}
	d, err := f.fenceOf(id)
	if err != nil {
		return mvmd.Deadline{}, err
	}
	if number > d.LastNumber {
		d = mvmd.Deadline{Phase: "armed", Armed: true, Length: length, Remaining: length, LastNumber: number}
		f.fences[id] = d
	}
	return d, nil
}

func (f *fakeRuntime) InspectMachineDeadline(_ context.Context, id string) (mvmd.Deadline, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fenceOf(id)
}

func (f *fakeRuntime) SnapshotMachine(_ context.Context, snapshotID, machineID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("snapshot " + snapshotID); err != nil {
		return err
	}
	m, ok := f.machines[machineID]
	if !ok {
		return mvmd.ErrNotFound
	}
	if (m.State != mvmd.StateRunning && m.State != mvmd.StatePaused) || m.BootSet != "" {
		return mvmd.ErrPrecondition
	}
	sn := mvmd.Snapshot{
		ID: snapshotID, MachineID: machineID, ImageDigest: m.ImageDigest, Driver: "fake 1", CPUModel: "model",
		RootDisk: m.RootDiskBytes, SizeBytes: 42,
	}
	for _, d := range m.Disks {
		sn.Disks = append(sn.Disks, mvmd.SnapshotDisk{ID: d.ID, ReadOnly: d.ReadOnly, SizeBytes: f.disks[d.ID].SizeBytes})
	}
	f.snapshots[snapshotID] = sn
	return nil
}

func (f *fakeRuntime) ListSnapshots(context.Context) ([]mvmd.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mvmd.Snapshot
	for _, s := range f.snapshots {
		out = append(out, s)
	}
	return out, f.err
}

func (f *fakeRuntime) DeleteSnapshot(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("deleteSnapshot " + id); err != nil {
		return err
	}
	delete(f.snapshots, id)
	return nil
}

func (f *fakeRuntime) RestoreMachine(_ context.Context, r mvmd.Restore) (mvmd.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("restore " + r.ID); err != nil {
		return mvmd.Machine{}, err
	}
	sn, ok := f.snapshots[r.SnapshotID]
	if !ok {
		return mvmd.Machine{}, mvmd.ErrNotFound
	}
	if _, ok := f.machines[r.ID]; ok {
		return mvmd.Machine{}, mvmd.ErrConflict
	}
	m := mvmd.Machine{
		ID: r.ID, State: mvmd.StateRunning, ImageDigest: sn.ImageDigest, RootDiskBytes: sn.RootDisk,
		Policy: r.Policy, RoutedPrefixes: r.RoutedPrefixes, Addresses: []string{"10.1.0.6"},
	}
	for _, d := range sn.Disks {
		if _, ok := f.disks[d.ID]; !ok {
			return mvmd.Machine{}, fmt.Errorf("disk %s is gone: %w", d.ID, mvmd.ErrPrecondition)
		}
		m.Disks = append(m.Disks, mvmd.Attachment{ID: d.ID, ReadOnly: d.ReadOnly})
	}
	f.machines[r.ID] = m
	return m, nil
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// AddBootSet reads both files whole and refuses one whose SHA-256 is not the set's, as mvmd does.
func (f *fakeRuntime) AddBootSet(_ context.Context, set mvmd.BootSet, kernel, initrd io.Reader) error {
	k, err := io.ReadAll(kernel)
	if err != nil {
		return err
	}
	i, err := io.ReadAll(initrd)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("addBootSet " + set.Name); err != nil {
		return err
	}
	if got := sha256Of(k); got != set.KernelDigest {
		return fmt.Errorf("%w: the kernel's SHA-256 is %s", mvmd.ErrInvalid, got)
	}
	if got := sha256Of(i); got != set.InitrdDigest {
		return fmt.Errorf("%w: the initrd's SHA-256 is %s", mvmd.ErrInvalid, got)
	}
	if have, ok := f.bootSets[set.Name]; ok && have != set {
		return mvmd.ErrConflict
	}
	f.bootSets[set.Name] = set
	f.bootFiles[set.Name] = [2][]byte{k, i}
	return nil
}

func (f *fakeRuntime) RemoveBootSet(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("removeBootSet " + name); err != nil {
		return err
	}
	for _, m := range f.machines {
		if m.BootSet == name {
			return mvmd.ErrPrecondition
		}
	}
	delete(f.bootSets, name)
	delete(f.bootFiles, name)
	return nil
}

// Exec runs a command of the fake's own: "cat" answers the guest files it names, "absent" does not
// start, "kill" ends by SIGTERM, "flood" writes 17 MiB and exits, "stop" is cut short, and any
// other runs until a signal ends it.
func (f *fakeRuntime) Exec(_ context.Context, machineID, execID string, command []string) (mvmd.ExecRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m, ok := f.machines[machineID]; !ok || m.State != mvmd.StateRunning {
		return mvmd.ExecRecord{}, fmt.Errorf("%w: %s is not running", mvmd.ErrPrecondition, machineID)
	}
	if err := f.step("exec " + machineID); err != nil {
		return mvmd.ExecRecord{}, err
	}
	x := &fakeExec{command: command, done: make(chan struct{})}
	f.execs[execID] = x
	out := func(stderr bool, b []byte) {
		var at uint64
		if n := len(x.output); n > 0 {
			at = x.output[n-1].Offset + uint64(len(x.output[n-1].Data))
		}
		x.output = append(x.output, mvmd.Chunk{Stderr: stderr, Offset: at, Data: b})
	}
	switch command[0] {
	case "absent":
		x.record = mvmd.ExecRecord{Status: mvmd.ExecNotStarted, Reason: "absent: no such file"}
		close(x.done)
		return x.record, nil
	case "cat":
		x.record.Status = mvmd.ExecExited
		for _, p := range command[1:] {
			if b, ok := f.guest[machineID][p]; ok {
				out(false, b)
			} else {
				out(true, []byte("cat: "+p+": No such file or directory\n"))
				x.record.ExitCode = 1
			}
		}
		close(x.done)
	case "kill":
		x.record = mvmd.ExecRecord{Status: mvmd.ExecExited, TermSignal: 15}
		close(x.done)
	case "flood":
		for range 17 {
			out(false, make([]byte, 1<<20))
		}
		x.record.Status = mvmd.ExecExited
		close(x.done)
	case "stop":
		x.record = mvmd.ExecRecord{Status: mvmd.ExecCutShort, Reason: "the Machine stopped"}
		close(x.done)
	default:
		out(false, []byte("started\n"))
	}
	return mvmd.ExecRecord{Status: mvmd.ExecStarted}, nil
}

func (f *fakeRuntime) ReadExecOutput(ctx context.Context, _, execID string, after uint64, sink func(mvmd.Chunk) error) (mvmd.ExecRecord, error) {
	f.mu.Lock()
	x, ok := f.execs[execID]
	f.mu.Unlock()
	if !ok {
		return mvmd.ExecRecord{}, mvmd.ErrNotFound
	}
	for _, c := range x.output {
		if c.Offset < after {
			continue
		}
		if err := sink(c); err != nil {
			return mvmd.ExecRecord{}, err
		}
	}
	select {
	case <-x.done:
		f.mu.Lock()
		defer f.mu.Unlock()
		return x.record, nil
	case <-ctx.Done():
		return mvmd.ExecRecord{}, ctx.Err()
	}
}

func (f *fakeRuntime) SignalExec(_ context.Context, _, execID string, signal int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.execs[execID]
	if !ok {
		return mvmd.ErrNotFound
	}
	x.signals = append(x.signals, signal)
	select {
	case <-x.done:
		return fmt.Errorf("%w: the command has ended", mvmd.ErrPrecondition)
	default:
	}
	x.record = mvmd.ExecRecord{Status: mvmd.ExecExited, TermSignal: signal}
	close(x.done)
	return nil
}

func (f *fakeRuntime) ReadFile(_ context.Context, machineID, path string, sink func([]byte) error) error {
	f.mu.Lock()
	b, ok := f.guest[machineID][path]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", mvmd.ErrNotFound, path)
	}
	for len(b) > 0 {
		n := min(len(b), 1<<20)
		if err := sink(b[:n]); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func (f *fakeRuntime) WriteFile(_ context.Context, machineID, path string, content io.Reader) error {
	b, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.step("write " + machineID + " " + path); err != nil {
		return err
	}
	if f.guest[machineID] == nil {
		f.guest[machineID] = map[string][]byte{}
	}
	f.guest[machineID][path] = b
	return nil
}
