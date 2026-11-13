package api

import (
	"context"
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
}

// step records a call that changes mvmd and returns the error a test set for it.
func (f *fakeRuntime) step(name string) error {
	f.calls = append(f.calls, name)
	return f.failAt[name]
}

func newFake() *fakeRuntime {
	return &fakeRuntime{
		machines: map[string]mvmd.Machine{}, disks: map[string]mvmd.Disk{}, failAt: map[string]error{}, contents: map[string][]byte{},
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
		BootSet: c.BootSet, Cmdline: c.Cmdline, RoutedPrefixes: c.RoutedPrefixes,
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
	m.State = mvmd.StateRunning
	f.machines[id] = m
	return m, nil
}

func (f *fakeRuntime) StopMachine(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
