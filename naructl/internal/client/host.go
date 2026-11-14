package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/garamsh/naru/naructl/internal/pki"
)

// HostPort is the port naru-hostd serves the host API on unless a client file's endpoint says another.
const HostPort = "7443"

// Roles of a host API client certificate.
const (
	Provisioner = "provisioner"
	HostReader  = "reader"
)

var hostOrganizations = map[string]string{Provisioner: "naru:host-provisioner", HostReader: "naru:host-reader"}

// IssueHost returns a client file for host APIs: its certificate, issued by the host authority with
// CN cn, carries the Organization of role. Each endpoint is an IP address, with a port or without.
func IssueHost(authority *pki.CA, role, cn string, endpoints []string) (File, error) {
	org, ok := hostOrganizations[role]
	if !ok {
		return File{}, fmt.Errorf("the role %q is not %s or %s", role, Provisioner, HostReader)
	}
	if len(endpoints) == 0 {
		return File{}, errors.New("a client file needs at least one endpoint")
	}
	f := File{CA: string(authority.CertPEM)}
	for _, e := range endpoints {
		hostPort, err := AddressPort(e, HostPort)
		if err != nil {
			return File{}, err
		}
		f.Endpoints = append(f.Endpoints, hostPort)
	}
	pair, err := authority.Client(cn, org)
	if err != nil {
		return File{}, err
	}
	f.Certificate, f.Key = string(pair.CertPEM), string(pair.KeyPEM)
	return f, nil
}

// HostClient calls one host's API.
type HostClient struct{ c *Client }

// DialHost returns a HostClient for the host at node, an IP address with or without a port; when
// node is empty it is the file's first endpoint.
func DialHost(f File, node string) (*HostClient, error) {
	c, err := DialPort(f, node, HostPort)
	return &HostClient{c}, err
}

// Rule, PortRange and NetworkPolicy are a microVM's network policy.
type Rule struct {
	Prefix   string      `json:"prefix"`
	Protocol string      `json:"protocol"`
	Ports    []PortRange `json:"ports,omitempty"`
}

// PortRange is first to last, inclusive.
type PortRange struct {
	First uint32 `json:"first"`
	Last  uint32 `json:"last"`
}

// NetworkPolicy is the rules a microVM's traffic is checked against.
type NetworkPolicy struct {
	Egress  []Rule `json:"egress,omitempty"`
	Ingress []Rule `json:"ingress,omitempty"`
}

// Disk is a disk of a microVM.
type Disk struct {
	Name      string `json:"name"`
	SizeBytes uint64 `json:"sizeBytes"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// MicroVMSpec is the body of POST /v1/microvms: the image form (Image, RootDiskBytes, Disks) or
// the OS form (BootSet, Cmdline, Seed, DataDisk, PodPrefix), not both.
type MicroVMSpec struct {
	Name          string        `json:"name"`
	Image         string        `json:"image,omitempty"`
	CPU           uint32        `json:"cpu"`
	Memory        uint64        `json:"memory"`
	RootDiskBytes uint64        `json:"rootDiskBytes,omitempty"`
	Disks         []Disk        `json:"disks,omitempty"`
	NetworkPolicy NetworkPolicy `json:"networkPolicy"`
	BootSet       string        `json:"bootSet,omitempty"`
	Cmdline       string        `json:"cmdline,omitempty"`
	// Seed is the user-data of the NoCloud seed, sent in base64.
	Seed      []byte `json:"seed,omitempty"`
	DataDisk  uint64 `json:"dataDisk,omitempty"`
	PodPrefix string `json:"podPrefix,omitempty"`
	// Network is the declared network the microVM is created on; absent, it is on none.
	Network string `json:"network,omitempty"`
	// RestartOnBoot is whether mvmd starts the microVM again after the host reboots; absent, the
	// host decides by the form (true for the OS form, false for the image form).
	RestartOnBoot *bool `json:"restartOnBoot,omitempty"`
}

// MicroVM is a microVM as the host API shows it.
type MicroVM struct {
	Name          string        `json:"name"`
	MachineID     string        `json:"machineId"`
	State         string        `json:"state"`
	Failure       string        `json:"failure,omitempty"`
	Addresses     []string      `json:"addresses"`
	ImageDigest   string        `json:"imageDigest"`
	CPU           uint32        `json:"cpu"`
	Memory        uint64        `json:"memory"`
	RootDiskBytes uint64        `json:"rootDiskBytes"`
	Disks         []Disk        `json:"disks"`
	NetworkPolicy NetworkPolicy `json:"networkPolicy"`
	BootSet       string        `json:"bootSet,omitempty"`
	Cmdline       string        `json:"cmdline,omitempty"`
	PodPrefix     string        `json:"podPrefix,omitempty"`
	RestartOnBoot bool          `json:"restartOnBoot"`
}

// HostInfo is GET /v1/host.
type HostInfo struct {
	Region   string            `json:"region"`
	Labels   map[string]string `json:"labels"`
	Capacity HostResources     `json:"capacity"`
	Free     HostResources     `json:"free"`
	Mvmd     struct {
		Version string `json:"version"`
		Driver  string `json:"driver"`
	} `json:"mvmd"`
}

// HostResources are amounts of CPU, memory (bytes) and state disk (bytes).
type HostResources struct {
	CPU    uint32 `json:"cpu"`
	Memory uint64 `json:"memory"`
	Disk   uint64 `json:"disk"`
}

// Host returns the host's region, labels and capacity.
func (h *HostClient) Host(ctx context.Context) (HostInfo, error) {
	var v HostInfo
	err := h.c.getJSON(ctx, "/v1/host", &v)
	return v, err
}

// Create creates the microVM, or returns the one an earlier request with the same body made. It
// reports whether it was made now.
func (h *HostClient) Create(ctx context.Context, spec MicroVMSpec) (MicroVM, bool, error) {
	resp, err := h.c.do(ctx, http.MethodPost, "/v1/microvms", spec)
	if err != nil {
		return MicroVM{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	var v MicroVM
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&v); err != nil {
		return MicroVM{}, false, fmt.Errorf("POST /v1/microvms: the answer is not what the host API sends: %w", err)
	}
	return v, resp.StatusCode == http.StatusCreated, nil
}

// Get returns one microVM.
func (h *HostClient) Get(ctx context.Context, name string) (MicroVM, error) {
	var v MicroVM
	err := h.c.getJSON(ctx, "/v1/microvms/"+url.PathEscape(name), &v)
	return v, err
}

// List returns every microVM, by name.
func (h *HostClient) List(ctx context.Context) ([]MicroVM, error) {
	var body struct {
		MicroVMs []MicroVM `json:"microvms"`
	}
	err := h.c.getJSON(ctx, "/v1/microvms", &body)
	return body.MicroVMs, err
}

// Stop stops the microVM, and returns it as it is; a stopped one is returned as it is.
func (h *HostClient) Stop(ctx context.Context, name string) (MicroVM, error) {
	return h.lifecycle(ctx, name, "stop")
}

// Start starts the microVM, and returns it as it is; a running one is returned as it is.
func (h *HostClient) Start(ctx context.Context, name string) (MicroVM, error) {
	return h.lifecycle(ctx, name, "start")
}

func (h *HostClient) lifecycle(ctx context.Context, name, verb string) (MicroVM, error) {
	path := "/v1/microvms/" + url.PathEscape(name) + "/" + verb
	resp, err := h.c.do(ctx, http.MethodPost, path, nil)
	if err != nil {
		return MicroVM{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var v MicroVM
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&v); err != nil {
		return MicroVM{}, fmt.Errorf("POST %s: the answer is not what the host API sends: %w", path, err)
	}
	return v, nil
}

// Delete stops and deletes the microVM and its disks.
func (h *HostClient) Delete(ctx context.Context, name string) error {
	resp, err := h.c.do(ctx, http.MethodDelete, "/v1/microvms/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Body.Close()
}
