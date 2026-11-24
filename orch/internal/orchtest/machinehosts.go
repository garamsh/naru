package orchtest

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// FakeMachineHosts stands in for the hosts' APIs as the Machine reconciler reaches them. Every orch
// member shares one, as the members share the real hosts. It answers as hostd does: a create of a
// name it holds is the microVM it holds, a missing microVM or disk snapshot is 404, and a stop or
// delete of one that is gone is no error to the reconciler.
type FakeMachineHosts struct {
	mu    sync.Mutex
	hosts map[string]*fakeMachineHost
	// DeleteDelay holds each delete of a microVM that long, so a delete outlasts a round trip of
	// the Machine API as a real host's does.
	DeleteDelay time.Duration
}

type fakeMachineHost struct {
	host    machines.Host
	down    bool
	vms     map[string]hostapi.MicroVM
	disks   map[string]hostapi.DiskSnapshot
	creates []hostapi.CreateRequest
}

var _ machines.Hosts = (*FakeMachineHosts)(nil)

// NewFakeMachineHosts returns the hosts, each answering with nothing on it.
func NewFakeMachineHosts(hosts ...machines.Host) *FakeMachineHosts {
	f := &FakeMachineHosts{hosts: map[string]*fakeMachineHost{}}
	for _, h := range hosts {
		f.hosts[h.ID] = &fakeMachineHost{host: h, vms: map[string]hostapi.MicroVM{}, disks: map[string]hostapi.DiskSnapshot{}}
	}
	return f
}

// Fail makes the host stop answering, and Recover answer again; what it holds stays.
func (f *FakeMachineHosts) Fail(id string)    { f.set(id, func(h *fakeMachineHost) { h.down = true }) }
func (f *FakeMachineHosts) Recover(id string) { f.set(id, func(h *fakeMachineHost) { h.down = false }) }

// Exit stops a microVM as if it stopped on its own, in state (stopped or faulted).
func (f *FakeMachineHosts) Exit(id, microVM, state string) {
	f.set(id, func(h *fakeMachineHost) {
		vm := h.vms[microVM]
		vm.State = state
		h.vms[microVM] = vm
	})
}

// Put places a microVM on the host behind the reconciler's back.
func (f *FakeMachineHosts) Put(id string, vm hostapi.MicroVM) {
	f.set(id, func(h *fakeMachineHost) { h.vms[vm.Name] = vm })
}

func (f *FakeMachineHosts) set(id string, change func(h *fakeMachineHost)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f.hosts[id])
}

// MicroVMs returns the microVMs on the host by name.
func (f *FakeMachineHosts) MicroVMs(id string) map[string]hostapi.MicroVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.hosts[id].vms)
}

// DiskSnapshots returns the names of the host's disk snapshots, in order.
func (f *FakeMachineHosts) DiskSnapshots(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.hosts[id].disks))
}

// Creates returns every create the host was asked, in order.
func (f *FakeMachineHosts) Creates(id string) []hostapi.CreateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hosts[id].creates)
}

func (f *FakeMachineHosts) List(context.Context) ([]machines.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []machines.Host
	for _, id := range slices.Sorted(maps.Keys(f.hosts)) {
		h := f.hosts[id]
		out = append(out, h.host)
		if h.down {
			continue
		}
		o := &out[len(out)-1]
		o.Answering = true
		for _, n := range slices.Sorted(maps.Keys(h.vms)) {
			o.MicroVMs = append(o.MicroVMs, h.vms[n])
		}
		for _, n := range slices.Sorted(maps.Keys(h.disks)) {
			o.DiskSnapshots = append(o.DiskSnapshots, h.disks[n])
		}
	}
	return out, nil
}

// answering returns the host when it answers; the caller holds f.mu.
func (f *FakeMachineHosts) answering(id string) (*fakeMachineHost, error) {
	h, ok := f.hosts[id]
	if !ok || h.down {
		return nil, fmt.Errorf("host %s does not answer", id)
	}
	return h, nil
}

func notFound(format string, a ...any) error {
	return &hostapi.Error{Status: http.StatusNotFound, Message: fmt.Sprintf(format, a...)}
}

func (f *FakeMachineHosts) Create(_ context.Context, host string, r hostapi.CreateRequest) (hostapi.MicroVM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return hostapi.MicroVM{}, err
	}
	h.creates = append(h.creates, r)
	if vm, ok := h.vms[r.Name]; ok {
		return vm, nil
	}
	for _, d := range r.Disks {
		if d.FromSnapshot != "" && h.disks[d.FromSnapshot].Name == "" {
			return hostapi.MicroVM{}, notFound("no disk snapshot %s", d.FromSnapshot)
		}
	}
	vm := hostapi.MicroVM{
		Name: r.Name, MachineID: r.Name, State: "running", ImageDigest: r.Image, CPU: r.CPU, Memory: r.Memory,
		RootDiskBytes: r.RootDiskBytes, Disks: r.Disks, NetworkPolicy: r.NetworkPolicy,
	}
	h.vms[r.Name] = vm
	return vm, nil
}

func (f *FakeMachineHosts) state(host, microVM, state string) (hostapi.MicroVM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return hostapi.MicroVM{}, err
	}
	vm, ok := h.vms[microVM]
	if !ok {
		return hostapi.MicroVM{}, notFound("no microVM %s", microVM)
	}
	vm.State = state
	h.vms[microVM] = vm
	return vm, nil
}

func (f *FakeMachineHosts) Start(_ context.Context, host, microVM string) (hostapi.MicroVM, error) {
	return f.state(host, microVM, "running")
}

func (f *FakeMachineHosts) Stop(_ context.Context, host, microVM string, _ uint32) (hostapi.MicroVM, error) {
	vm, err := f.state(host, microVM, "stopped")
	if he, ok := err.(*hostapi.Error); ok && he.Status == http.StatusNotFound {
		return hostapi.MicroVM{Name: microVM, State: "stopped"}, nil
	}
	return vm, err
}

func (f *FakeMachineHosts) Delete(ctx context.Context, host, microVM string) error {
	f.mu.Lock()
	_, err := f.answering(host)
	f.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(f.DeleteDelay):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hosts[host].vms, microVM)
	return nil
}

func (f *FakeMachineHosts) SnapshotDisk(_ context.Context, host, microVM, disk, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return err
	}
	vm, ok := h.vms[microVM]
	if !ok {
		return notFound("no microVM %s", microVM)
	}
	i := slices.IndexFunc(vm.Disks, func(d hostapi.DiskSpec) bool { return d.Name == disk })
	if i < 0 {
		return notFound("the microVM %s has no disk %s", microVM, disk)
	}
	if _, ok := h.disks[name]; !ok {
		size := vm.Disks[i].SizeBytes
		h.disks[name] = hostapi.DiskSnapshot{Name: name, MicroVM: microVM, Disk: disk, SizeBytes: size, AllocatedBytes: size}
	}
	return nil
}

func (f *FakeMachineHosts) DeleteDiskSnapshot(_ context.Context, host, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.answering(host)
	if err != nil {
		return err
	}
	delete(h.disks, name)
	return nil
}
