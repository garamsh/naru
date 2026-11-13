package api

import (
	"context"
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
}

func newFake() *fakeRuntime {
	return &fakeRuntime{
		machines: map[string]mvmd.Machine{}, disks: map[string]mvmd.Disk{},
		info: mvmd.Info{APIVersion: "v1alpha2", Driver: "fake", CPUs: 8, MemoryBytes: 1 << 33, StateTotalBytes: 100, StateAvailableBytes: 60},
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
	if _, ok := f.disks[id]; ok {
		return mvmd.ErrConflict
	}
	f.disks[id] = mvmd.Disk{ID: id, SizeBytes: size}
	return f.err
}

func (f *fakeRuntime) DeleteDisk(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	if f.err != nil {
		return mvmd.Machine{}, f.err
	}
	m := mvmd.Machine{
		ID: c.ID, State: mvmd.StateCreated, ImageDigest: c.ImageDigest, CPU: c.CPU, MemoryBytes: c.MemoryBytes,
		RootDiskBytes: c.RootDiskBytes, Disks: c.Disks, Policy: c.Policy, Addresses: []string{"10.1.0.5"},
	}
	f.machines[c.ID] = m
	return m, nil
}

func (f *fakeRuntime) StartMachine(_ context.Context, id string) (mvmd.Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
