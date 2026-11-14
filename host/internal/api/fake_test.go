package api

import (
	"context"
	"fmt"
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
	// stops counts the StopMachine calls that reached mvmd.
	stops int
	// fences is the state of each fenced Machine's own fence; snapshots are the snapshots held.
	fences    map[string]mvmd.Deadline
	snapshots map[string]mvmd.Snapshot
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
		info: mvmd.Info{
			APIVersion: "v1alpha2", Driver: "fake", CPUs: 8, MemoryBytes: 1 << 33, StateTotalBytes: 100, StateAvailableBytes: 60,
			MachinePrefixes: []string{"10.1.0.0/24"},
		},
	}
}

func (f *fakeRuntime) Info(context.Context) (mvmd.Info, error) { return f.info, f.err }

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
	}
	f.machines[c.ID] = m
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

func (f *fakeRuntime) StopMachine(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	m, ok := f.machines[id]
	if !ok {
		return mvmd.ErrNotFound
	}
	m.State = mvmd.StateStopped
	f.machines[id] = m
	return nil
}

func (f *fakeRuntime) DeleteMachine(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
