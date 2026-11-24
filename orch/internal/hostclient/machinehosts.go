package hostclient

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"sync"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// MachineHosts is the Machine reconciler's view of the hosts: each admitted host's API at the
// address the inventory recorded, as Client reaches it.
//
// What a host reports is read through a probe that tolerates probeFailures-1 failures in a row by
// answering with the last answer, so one lost probe does not show the host's Machines unknown.
type MachineHosts struct {
	c       *Client
	reports probes[report]
}

// report is what one host answered in one probe.
type report struct {
	info     hostapi.Host
	microVMs []hostapi.MicroVM
	disks    []hostapi.DiskSnapshot
}

var _ machines.Hosts = (*MachineHosts)(nil)

// NewMachineHosts returns the reconciler's view of the hosts of inv, dialled as New dials them.
func NewMachineHosts(inv Inventory, hc *http.Client, revocations *identity.RevocationList) *MachineHosts {
	return &MachineHosts{c: New(inv, hc, revocations), reports: probes[report]{by: map[string]*probed[report]{}}}
}

// List returns every admitted host. One that does not answer is listed with Answering false and
// nothing it reports. The hosts it read are the ones the calls that follow, until the next List,
// reach.
func (m *MachineHosts) List(ctx context.Context) ([]machines.Host, error) {
	admitted, err := m.c.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	pass := make(map[string]orchapi.Host, len(admitted))
	for _, h := range admitted {
		pass[h.ID] = h
	}
	m.c.mu.Lock()
	m.c.pass = pass
	m.c.mu.Unlock()
	m.reports.keep(pass)
	out := make([]machines.Host, len(admitted))
	var wg sync.WaitGroup
	for i, h := range admitted {
		out[i] = machines.Host{ID: h.ID, Region: h.Region, Labels: h.Labels, Draining: h.Draining}
		wg.Go(func() {
			r, err := m.reports.get(h.ID, func() (report, error) { return m.probe(ctx, h) })
			if err != nil {
				return
			}
			out[i].Answering = true
			out[i].Reported = r.info.ReportedLabels
			out[i].Capacity = r.info.Capacity
			out[i].MicroVMs = r.microVMs
			out[i].DiskSnapshots = r.disks
		})
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// probe reads the host, its microVMs and its disk snapshots; any that fails fails the probe.
func (m *MachineHosts) probe(ctx context.Context, h orchapi.Host) (report, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	api := m.c.api(h)
	var r report
	var err error
	if r.info, err = api.Host(ctx); err != nil {
		return report{}, err
	}
	if r.microVMs, err = api.MicroVMs(ctx); err != nil {
		return report{}, err
	}
	if r.disks, err = api.DiskSnapshots(ctx); err != nil {
		return report{}, err
	}
	return r, nil
}

func (m *MachineHosts) Create(ctx context.Context, host string, r hostapi.CreateRequest) (hostapi.MicroVM, error) {
	api, ctx, cancel, err := m.c.host(ctx, host)
	if err != nil {
		return hostapi.MicroVM{}, err
	}
	defer cancel()
	return api.CreateMicroVM(ctx, r)
}

func (m *MachineHosts) Start(ctx context.Context, host, microVM string) (hostapi.MicroVM, error) {
	return m.vm(ctx, host, false, func(ctx context.Context, a *hostapi.Client) (hostapi.MicroVM, error) {
		return a.StartMicroVM(ctx, microVM)
	})
}

func (m *MachineHosts) Stop(ctx context.Context, host, microVM string, graceSeconds uint32) (hostapi.MicroVM, error) {
	return m.vm(ctx, host, true, func(ctx context.Context, a *hostapi.Client) (hostapi.MicroVM, error) {
		return a.StopMicroVM(ctx, microVM, hostapi.StopRequest{GraceSeconds: graceSeconds})
	})
}

func (m *MachineHosts) Delete(ctx context.Context, host, microVM string) error {
	return m.c.act(ctx, host, true, func(ctx context.Context, a *hostapi.Client) error { return a.DeleteMicroVM(ctx, microVM) })
}

func (m *MachineHosts) SnapshotDisk(ctx context.Context, host, microVM, disk, name string) error {
	return m.c.act(ctx, host, false, func(ctx context.Context, a *hostapi.Client) error {
		_, err := a.SnapshotDisk(ctx, microVM, disk, hostapi.DiskSnapshotRequest{Name: name})
		return err
	})
}

func (m *MachineHosts) DeleteDiskSnapshot(ctx context.Context, host, name string) error {
	return m.c.act(ctx, host, true, func(ctx context.Context, a *hostapi.Client) error { return a.DeleteDiskSnapshot(ctx, name) })
}

// vm is a call that answers the microVM; one that is gone answers the zero microVM when missingOK.
func (m *MachineHosts) vm(ctx context.Context, host string, missingOK bool, call func(context.Context, *hostapi.Client) (hostapi.MicroVM, error)) (hostapi.MicroVM, error) {
	var out hostapi.MicroVM
	err := m.c.act(ctx, host, false, func(ctx context.Context, a *hostapi.Client) error {
		var err error
		out, err = call(ctx, a)
		return err
	})
	var he *hostapi.Error
	if missingOK && errors.As(err, &he) && he.Status == http.StatusNotFound {
		return hostapi.MicroVM{State: "stopped"}, nil
	}
	return out, err
}
