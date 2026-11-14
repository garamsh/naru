package orchtest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/garamsh/naru/orch/internal/groups"
)

// FakeHosts stands in for the hosts' APIs. Every orch member shares one, as the members share the
// real hosts, so a microVM it made is seen by whichever member leads. It supports no fence.
type FakeHosts struct {
	mu    sync.Mutex
	hosts map[string]*fakeHost
}

type fakeHost struct {
	down     bool
	machines map[string]groups.MachineState
	// specs are what each machine was made from.
	specs map[string]groups.MachineSpec
}

var _ groups.Hosts = (*FakeHosts)(nil)

// NewFakeHosts returns hosts with the given IDs, all ready in region kr-seoul.
func NewFakeHosts(ids ...string) *FakeHosts {
	f := &FakeHosts{hosts: map[string]*fakeHost{}}
	for _, id := range ids {
		f.hosts[id] = &fakeHost{machines: map[string]groups.MachineState{}, specs: map[string]groups.MachineSpec{}}
	}
	return f
}

// Fail makes the host stop answering; its microVMs stay where they are.
func (f *FakeHosts) Fail(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts[id].down = true
}

// Remove deletes a microVM behind the engine's back.
func (f *FakeHosts) Remove(hostID, machineID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hosts[hostID].machines, machineID)
	delete(f.hosts[hostID].specs, machineID)
}

// Running returns the microVMs that run on hosts that answer, as "<host>/<machine>" in order.
func (f *FakeHosts) Running() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, h := range f.hosts {
		if h.down {
			continue
		}
		for m, st := range h.machines {
			if st == groups.MachineRunning {
				out = append(out, id+"/"+m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Hosts returns the ID of each host that holds the machine, running or not.
func (f *FakeHosts) Hosts(machineID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, h := range f.hosts {
		if _, ok := h.machines[machineID]; ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// List implements groups.Hosts.
func (f *FakeHosts) List(context.Context) ([]groups.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []groups.Host
	for id, h := range f.hosts {
		g := groups.Host{ID: id, Region: "kr-seoul", Labels: map[string]string{}, Ready: !h.down, Capacity: groups.Capacity{CPUs: 64, MemoryMiB: 65536}}
		if !h.down {
			for m, st := range h.machines {
				t := h.specs[m].Template
				mc := groups.Machine{ID: m, State: st, Network: t.Network, PodPrefix: h.specs[m].PodPrefix}
				if t.Form == groups.FormImage {
					mc.Image = t.Version
				} else {
					mc.BootSet = t.NodeVersion
				}
				g.Machines = append(g.Machines, mc)
			}
		}
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (f *FakeHosts) up(id string) (*fakeHost, error) {
	h, ok := f.hosts[id]
	if !ok || h.down {
		return nil, fmt.Errorf("host %s does not answer", id)
	}
	return h, nil
}

// Create implements groups.Hosts.
func (f *FakeHosts) Create(_ context.Context, hostID string, spec groups.MachineSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.up(hostID)
	if err != nil {
		return err
	}
	if _, ok := h.machines[spec.ID]; !ok {
		h.machines[spec.ID] = groups.MachineRunning
		h.specs[spec.ID] = spec
	}
	return nil
}

func (f *FakeHosts) set(hostID, machineID string, st groups.MachineState, remove bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, err := f.up(hostID)
	if err != nil {
		return err
	}
	if _, ok := h.machines[machineID]; !ok {
		return nil
	}
	if remove {
		delete(h.machines, machineID)
		delete(h.specs, machineID)
		return nil
	}
	h.machines[machineID] = st
	return nil
}

// Delete implements groups.Hosts.
func (f *FakeHosts) Delete(_ context.Context, hostID, machineID string) error {
	return f.set(hostID, machineID, "", true)
}

// Stop implements groups.Hosts.
func (f *FakeHosts) Stop(_ context.Context, hostID, machineID string) error {
	return f.set(hostID, machineID, groups.MachineStopped, false)
}

// Start implements groups.Hosts.
func (f *FakeHosts) Start(_ context.Context, hostID, machineID string) error {
	return f.set(hostID, machineID, groups.MachineRunning, false)
}

// RenewFence implements groups.Hosts.
func (f *FakeHosts) RenewFence(context.Context, string, string, time.Time) error {
	return fmt.Errorf("the fake hosts have no fence")
}

// InspectFence implements groups.Hosts.
func (f *FakeHosts) InspectFence(context.Context, string, string) (time.Time, time.Time, error) {
	return time.Time{}, time.Time{}, fmt.Errorf("the fake hosts have no fence")
}
