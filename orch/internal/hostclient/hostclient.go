// Package hostclient is the groups engine's view of the hosts: it drives each admitted host through
// that host's API (/v1/host and /v1/microvms) at the address the inventory recorded.
//
// The host API does not yet carry a per-Machine fence, so a stateful group's Create, RenewFence
// and InspectFence are refused with ErrFenceUnsupported rather than faked.
package hostclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
)

// ErrFenceUnsupported is returned for every fence operation: the host API has none yet.
var ErrFenceUnsupported = errors.New("the host API has no per-Machine fence yet")

// RootDiskBytes is the root disk of an image-form microVM: a group template does not size it.
const RootDiskBytes = 10 << 30

// seed is the user-data of an OS-form microVM: a group template carries none yet.
const seed = "#cloud-config\n"

const (
	probeTimeout = 10 * time.Second
	callTimeout  = 5 * time.Minute
	maxBody      = 16 << 20
)

// Client implements groups.Hosts.
type Client struct {
	inv  Inventory
	http *http.Client
}

var _ groups.Hosts = (*Client)(nil)

// New returns a client that dials the hosts of inv through hc, which carries orch's client
// identity for the host API.
func New(inv Inventory, hc *http.Client) *Client { return &Client{inv: inv, http: hc} }

type hostInfo struct {
	Free struct {
		CPU    int    `json:"cpu"`
		Memory uint64 `json:"memory"`
	} `json:"free"`
	Mvmd struct {
		Kernels []struct {
			Name string `json:"name"`
		} `json:"kernels"`
	} `json:"mvmd"`
}

type microVM struct {
	Name  string `json:"name"`
	State string `json:"state"`
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
	var info hostInfo
	var list struct {
		MicroVMs []microVM `json:"microvms"`
	}
	if c.do(ctx, h.Address, http.MethodGet, "/v1/host", nil, &info) != nil ||
		c.do(ctx, h.Address, http.MethodGet, "/v1/microvms", nil, &list) != nil {
		return
	}
	out.Ready = true
	out.Capacity = groups.Capacity{CPUs: info.Free.CPU, MemoryMiB: int(info.Free.Memory >> 20)}
	for _, k := range info.Mvmd.Kernels {
		out.NodeVersions = append(out.NodeVersions, k.Name)
	}
	for _, m := range list.MicroVMs {
		out.Machines = append(out.Machines, groups.Machine{ID: m.Name, State: machineState(m.State)})
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

type rule struct {
	Prefix   string      `json:"prefix"`
	Protocol string      `json:"protocol"`
	Ports    []portRange `json:"ports,omitempty"`
}

type portRange struct {
	First int `json:"first"`
	Last  int `json:"last"`
}

type disk struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type policy struct {
	Egress  []rule `json:"egress,omitempty"`
	Ingress []rule `json:"ingress,omitempty"`
}

type createRequest struct {
	Name          string `json:"name"`
	Image         string `json:"image,omitempty"`
	CPU           uint32 `json:"cpu"`
	Memory        uint64 `json:"memory"`
	RootDiskBytes uint64 `json:"rootDiskBytes,omitempty"`
	Disks         []disk `json:"disks,omitempty"`
	NetworkPolicy policy `json:"networkPolicy"`
	BootSet       string `json:"bootSet,omitempty"`
	Seed          []byte `json:"seed,omitempty"`
	DataDisk      uint64 `json:"dataDisk,omitempty"`
}

func rules(in []groups.Rule) []rule {
	var out []rule
	for _, r := range in {
		x := rule{Prefix: r.Prefix, Protocol: r.Protocol}
		for _, p := range r.Ports {
			x.Ports = append(x.Ports, portRange{First: p, Last: p})
		}
		out = append(out, x)
	}
	return out
}

// request is the host API's body for spec, in the form the template names.
func request(spec groups.MachineSpec) (createRequest, error) {
	t := spec.Template
	r := createRequest{
		Name: spec.ID, CPU: uint32(t.CPUs), Memory: uint64(t.MemoryMiB) << 20,
		NetworkPolicy: policy{Egress: rules(t.Network.Egress), Ingress: rules(t.Network.Ingress)},
	}
	switch t.Form {
	case groups.FormImage:
		r.Image, r.RootDiskBytes = t.Version, RootDiskBytes
		for _, d := range t.Disks {
			r.Disks = append(r.Disks, disk{Name: d.Name, SizeBytes: d.SizeBytes, ReadOnly: d.ReadOnly})
		}
	case groups.FormOS:
		if len(t.Disks) == 0 {
			return createRequest{}, errors.New("an OS-form template needs a disk: its first is the microVM's data disk")
		}
		r.BootSet, r.Seed, r.DataDisk = t.NodeVersion, []byte(seed), uint64(t.Disks[0].SizeBytes)
	default:
		return createRequest{}, fmt.Errorf("template form %q is neither image nor os", t.Form)
	}
	return r, nil
}

// address is the host's API address.
func (c *Client) address(ctx context.Context, hostID string) (string, error) {
	hs, err := c.inv.Hosts(ctx)
	if err != nil {
		return "", err
	}
	for _, h := range hs {
		if h.ID == hostID {
			return h.Address, nil
		}
	}
	return "", fmt.Errorf("host %s is not admitted", hostID)
}

// Create asks the host to make and start the microVM.
func (c *Client) Create(ctx context.Context, hostID string, spec groups.MachineSpec) error {
	if spec.Fenced {
		return ErrFenceUnsupported
	}
	body, err := request(spec)
	if err != nil {
		return err
	}
	addr, err := c.address(ctx, hostID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	return c.do(ctx, addr, http.MethodPost, "/v1/microvms", body, nil)
}

// Delete removes the microVM; one that is already gone is not an error, nor is stopping one.
func (c *Client) Delete(ctx context.Context, hostID, machineID string) error {
	return c.act(ctx, hostID, http.MethodDelete, "/v1/microvms/"+url.PathEscape(machineID), true)
}

// Stop stops the microVM.
func (c *Client) Stop(ctx context.Context, hostID, machineID string) error {
	return c.act(ctx, hostID, http.MethodPost, "/v1/microvms/"+url.PathEscape(machineID)+"/stop", true)
}

// Start starts the microVM.
func (c *Client) Start(ctx context.Context, hostID, machineID string) error {
	return c.act(ctx, hostID, http.MethodPost, "/v1/microvms/"+url.PathEscape(machineID)+"/start", false)
}

func (c *Client) act(ctx context.Context, hostID, method, path string, missingOK bool) error {
	addr, err := c.address(ctx, hostID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	err = c.do(ctx, addr, method, path, nil, nil)
	var he *Error
	if errors.As(err, &he) && he.Status == http.StatusNotFound && missingOK {
		return nil
	}
	return err
}

// RenewFence is not supported by the host API yet.
func (c *Client) RenewFence(context.Context, string, string, time.Time) error {
	return ErrFenceUnsupported
}

// InspectFence is not supported by the host API yet.
func (c *Client) InspectFence(context.Context, string, string) (time.Time, time.Time, error) {
	return time.Time{}, time.Time{}, ErrFenceUnsupported
}

// Error is a host's refusal.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("host answered %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, addr, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	u := url.URL{Scheme: "https", Host: addr, Path: path}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}
		var msg struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &msg) == nil {
			e.Message = msg.Error
		}
		return e
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
