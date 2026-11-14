// Package hostclient is the groups engine's view of the hosts: it drives each admitted host through
// that host's API at the address the inventory recorded, with lib/hostapi.
//
// A stateful member's fence is the host API's per-microVM fence: RenewFence sets its length from
// the deadline orch asked for, and InspectFence reads the time it has left and when it lapsed.
package hostclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
)

const (
	probeTimeout = 10 * time.Second
	callTimeout  = 5 * time.Minute
)

// Client implements groups.Hosts.
type Client struct {
	inv  Inventory
	http *http.Client
	// Now is the clock the fence lengths are measured on; it defaults to time.Now.
	Now func() time.Time
}

var _ groups.Hosts = (*Client)(nil)

// New returns a client that dials the hosts of inv through hc, which carries orch's client
// identity for the host API.
func New(inv Inventory, hc *http.Client) *Client { return &Client{inv: inv, http: hc, Now: time.Now} }

// api is the host API at addr.
func (c *Client) api(addr string) *hostapi.Client {
	return hostapi.NewClient((&url.URL{Scheme: "https", Host: addr}).String(), c.http)
}

// List returns every admitted host. A host that does not answer is listed as not ready.
func (c *Client) List(ctx context.Context) ([]groups.Host, error) {
	admitted, err := c.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]groups.Host, len(admitted))
	var wg sync.WaitGroup
	for i, h := range admitted {
		out[i] = groups.Host{ID: h.ID, Region: h.Region, Labels: h.Labels}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.probe(ctx, h, &out[i])
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// probe fills in what the host reports; the host stays not ready when either call fails.
func (c *Client) probe(ctx context.Context, h orchapi.Host, out *groups.Host) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	api := c.api(h.Address)
	info, err := api.Host(ctx)
	if err != nil {
		return
	}
	vms, err := api.MicroVMs(ctx)
	if err != nil {
		return
	}
	out.Ready = true
	out.Capacity = groups.Capacity{CPUs: int(info.Free.CPU), MemoryMiB: int(info.Free.Memory >> 20)}
	for _, k := range info.Mvmd.Kernels {
		out.NodeVersions = append(out.NodeVersions, k.Name)
	}
	for _, m := range vms {
		out.Machines = append(out.Machines, groups.Machine{
			ID: m.Name, State: machineState(m.State), Image: m.ImageDigest, BootSet: m.BootSet,
			Network: m.Network, PodPrefix: m.PodPrefix, Addresses: m.Addresses,
		})
	}
}

func machineState(s string) groups.MachineState {
	switch s {
	case "running", "paused":
		return groups.MachineRunning
	case "created":
		return groups.MachineCreated
	}
	return groups.MachineStopped
}

func rules(in []groups.Rule) []hostapi.Rule {
	var out []hostapi.Rule
	for _, r := range in {
		x := hostapi.Rule{Prefix: r.Prefix, Protocol: r.Protocol}
		for _, p := range r.Ports {
			x.Ports = append(x.Ports, hostapi.PortRange{First: uint32(p), Last: uint32(p)})
		}
		out = append(out, x)
	}
	return out
}

// request is the host API's body for spec, in the form the template names.
func request(spec groups.MachineSpec) (hostapi.CreateRequest, error) {
	t := spec.Template
	r := hostapi.CreateRequest{
		Name: spec.ID, CPU: uint32(t.CPUs), Memory: uint64(t.MemoryMiB) << 20, Fenced: spec.Fenced,
		Network: t.Network, PodPrefix: spec.PodPrefix,
		NetworkPolicy: hostapi.Policy{Egress: rules(t.Policy.Egress), Ingress: rules(t.Policy.Ingress)},
	}
	switch t.Form {
	case groups.FormImage:
		if t.RootDiskBytes <= 0 {
			return hostapi.CreateRequest{}, errors.New("an image-form template needs rootDiskBytes above zero")
		}
		r.Image, r.RootDiskBytes = t.Version, uint64(t.RootDiskBytes)
		for _, d := range t.Disks {
			r.Disks = append(r.Disks, hostapi.DiskSpec{Name: d.Name, SizeBytes: uint64(d.SizeBytes), ReadOnly: d.ReadOnly})
		}
	case groups.FormOS:
		if len(t.Disks) == 0 {
			return hostapi.CreateRequest{}, errors.New("an OS-form template needs a disk: its first is the microVM's data disk")
		}
		userData := t.UserData
		if userData == "" {
			userData = groups.DefaultUserData
		}
		r.BootSet, r.Seed, r.DataDisk = t.NodeVersion, []byte(userData), uint64(t.Disks[0].SizeBytes)
	default:
		return hostapi.CreateRequest{}, fmt.Errorf("template form %q is neither image nor os", t.Form)
	}
	return r, nil
}

// host is the API of the admitted host hostID, with the context of one call.
func (c *Client) host(ctx context.Context, hostID string) (*hostapi.Client, context.Context, context.CancelFunc, error) {
	hs, err := c.inv.Hosts(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, h := range hs {
		if h.ID == hostID {
			ctx, cancel := context.WithTimeout(ctx, callTimeout)
			return c.api(h.Address), ctx, cancel, nil
		}
	}
	return nil, nil, nil, fmt.Errorf("host %s is not admitted", hostID)
}

// Create asks the host to make the microVM. The host starts it unless it is fenced; a fenced one
// is started after its fence is armed.
func (c *Client) Create(ctx context.Context, hostID string, spec groups.MachineSpec) error {
	body, err := request(spec)
	if err != nil {
		return err
	}
	api, ctx, cancel, err := c.host(ctx, hostID)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = api.CreateMicroVM(ctx, body)
	return err
}

// Delete removes the microVM; one that is already gone is not an error, nor is stopping one.
func (c *Client) Delete(ctx context.Context, hostID, machineID string) error {
	return c.act(ctx, hostID, true, func(ctx context.Context, a *hostapi.Client) error { return a.DeleteMicroVM(ctx, machineID) })
}

// Stop stops the microVM.
func (c *Client) Stop(ctx context.Context, hostID, machineID string) error {
	return c.act(ctx, hostID, true, func(ctx context.Context, a *hostapi.Client) error {
		_, err := a.StopMicroVM(ctx, machineID)
		return err
	})
}

// Start starts the microVM.
func (c *Client) Start(ctx context.Context, hostID, machineID string) error {
	return c.act(ctx, hostID, false, func(ctx context.Context, a *hostapi.Client) error {
		_, err := a.StartMicroVM(ctx, machineID)
		return err
	})
}

func (c *Client) act(ctx context.Context, hostID string, missingOK bool, call func(context.Context, *hostapi.Client) error) error {
	api, ctx, cancel, err := c.host(ctx, hostID)
	if err != nil {
		return err
	}
	defer cancel()
	err = call(ctx, api)
	var he *hostapi.Error
	if errors.As(err, &he) && he.Status == http.StatusNotFound && missingOK {
		return nil
	}
	return err
}

// RenewFence sets the microVM's fence to end at deadline. The length is rounded down to whole
// seconds, so the fence ends later than deadline by no more than the call's own latency; the
// renewal number is the deadline's Unix time in nanoseconds, which grows with every renewal, also
// across leaders.
func (c *Client) RenewFence(ctx context.Context, hostID, machineID string, deadline time.Time) error {
	length := deadline.Sub(c.Now())
	if length < time.Second {
		return fmt.Errorf("fence deadline %s is less than a second away", deadline.Format(time.RFC3339))
	}
	api, ctx, cancel, err := c.host(ctx, hostID)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = api.RenewFence(ctx, machineID, hostapi.FenceRequest{LengthSeconds: uint64(length / time.Second), Renewal: uint64(deadline.UnixNano())})
	return err
}

// InspectFence returns when the fence ends, as the time it reports left counted from now, and when
// it lapsed; the first is zero unless the fence is armed, the second until it has lapsed.
func (c *Client) InspectFence(ctx context.Context, hostID, machineID string) (time.Time, time.Time, error) {
	api, ctx, cancel, err := c.host(ctx, hostID)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	defer cancel()
	now := c.Now()
	f, err := api.Fence(ctx, machineID)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	vm, err := api.MicroVM(ctx, machineID)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	var deadline time.Time
	if f.Phase == hostapi.FenceArmed {
		deadline = now.Add(time.Duration(f.RemainingSeconds) * time.Second)
	}
	return deadline, vm.FenceLapsedAt, nil
}
