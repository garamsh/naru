package hostapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxResponse bounds a response body the client reads: an exec's MaxGuestBytes of output, base64
// in JSON, with room for the rest of its answer.
const maxResponse = 2 * MaxGuestBytes

// Error is a refusal or failure the host API answered: its status, and the message and the fields
// it names. Read it with errors.As.
type Error struct {
	Status  int
	Message string
	Fields  []string
	Code    string
}

func (e *Error) Error() string {
	if len(e.Fields) > 0 {
		return fmt.Sprintf("host API %d: %s (%s)", e.Status, e.Message, strings.Join(e.Fields, ", "))
	}
	return fmt.Sprintf("host API %d: %s", e.Status, e.Message)
}

// Client calls one host's API. The caller supplies the http.Client, which carries its identity: a
// TLS configuration with the client certificate and the host authority's CA.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a Client for the API at base, such as "https://10.0.0.1:7443".
func NewClient(base string, hc *http.Client) *Client {
	return &Client{base: strings.TrimSuffix(base, "/"), http: hc}
}

// do sends in as JSON when it is not nil and decodes a JSON answer into out when it is not nil.
// An answer of 400 or above is an *Error.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	_, err := c.send(ctx, method, path, in, out)
	return err
}

// send is do that also returns the status of the answer.
func (c *Client) send(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	contentType := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body, contentType = bytes.NewReader(b), "application/json"
	}
	return c.exchange(ctx, method, path, body, contentType, out)
}

// exchange sends body, whatever it is, and decodes a JSON answer into out when it is not nil.
func (c *Client) exchange(ctx context.Context, method, path string, body io.Reader, contentType string, out any) (int, error) {
	status, data, err := c.fetch(ctx, method, path, body, contentType)
	if err != nil || out == nil || len(data) == 0 {
		return status, err
	}
	return status, json.Unmarshal(data, out)
}

// fetch sends body and returns the answer's body, whatever it is. An answer of 400 or above is an
// *Error.
func (c *Client) fetch(ctx context.Context, method, path string, body io.Reader, contentType string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		var eb ErrorBody
		if json.Unmarshal(data, &eb) != nil || eb.Error == "" {
			eb.Error = http.StatusText(resp.StatusCode)
		}
		return resp.StatusCode, nil, &Error{Status: resp.StatusCode, Message: eb.Error, Fields: eb.Fields, Code: eb.Code}
	}
	return resp.StatusCode, data, nil
}

func microVMPath(name string) string { return "/v1/microvms/" + url.PathEscape(name) }

func snapshotPath(name string) string { return "/v1/snapshots/" + url.PathEscape(name) }

func networkPath(name string) string { return "/v1/networks/" + url.PathEscape(name) }

// PutNetwork declares the network, or replaces the one of its name, and returns it.
func (c *Client) PutNetwork(ctx context.Context, name string, r NetworkRequest) (Network, error) {
	var n Network
	err := c.do(ctx, http.MethodPut, networkPath(name), r, &n)
	return n, err
}

// Network returns the network named name.
func (c *Client) Network(ctx context.Context, name string) (Network, error) {
	var n Network
	err := c.do(ctx, http.MethodGet, networkPath(name), nil, &n)
	return n, err
}

// Networks lists the networks, ordered by name.
func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var out struct {
		Networks []Network `json:"networks"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/networks", nil, &out)
	return out.Networks, err
}

// DeleteNetwork removes the network and detaches its microVMs from it.
func (c *Client) DeleteNetwork(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, networkPath(name), nil, nil)
}

// SetPeers replaces the tunnel's peers and the host's mesh address, and returns what the host holds.
func (c *Client) SetPeers(ctx context.Context, req PeersRequest) (PeersRequest, error) {
	var out PeersRequest
	err := c.do(ctx, http.MethodPut, "/v1/peers", req, &out)
	return out, err
}

// Peers returns the tunnel's peers and the host's mesh address.
func (c *Client) Peers(ctx context.Context) (PeersRequest, error) {
	var out PeersRequest
	err := c.do(ctx, http.MethodGet, "/v1/peers", nil, &out)
	return out, err
}

// Tunnel returns the host's tunnel public key and port, and the Machine prefixes its microVMs'
// addresses are in.
func (c *Client) Tunnel(ctx context.Context) (Tunnel, error) {
	var t Tunnel
	err := c.do(ctx, http.MethodGet, "/v1/tunnel", nil, &t)
	return t, err
}

// Host returns the host's region, labels, capacity and mvmd's version.
func (c *Client) Host(ctx context.Context) (Host, error) {
	var h Host
	err := c.do(ctx, http.MethodGet, "/v1/host", nil, &h)
	return h, err
}

// CreateMicroVM creates a microVM and starts it, unless it is fenced. A repeat of the same request
// answers the microVM as it is.
func (c *Client) CreateMicroVM(ctx context.Context, r CreateRequest) (MicroVM, error) {
	var m MicroVM
	err := c.do(ctx, http.MethodPost, "/v1/microvms", r, &m)
	return m, err
}

// CreateMicroVMReport is CreateMicroVM that also reports whether the microVM was made by this
// request, and not by an earlier one with the same body.
func (c *Client) CreateMicroVMReport(ctx context.Context, r CreateRequest) (m MicroVM, created bool, err error) {
	status, err := c.send(ctx, http.MethodPost, "/v1/microvms", r, &m)
	return m, status == http.StatusCreated, err
}

// MicroVM returns the microVM named name.
func (c *Client) MicroVM(ctx context.Context, name string) (MicroVM, error) {
	var m MicroVM
	err := c.do(ctx, http.MethodGet, microVMPath(name), nil, &m)
	return m, err
}

// MicroVMs lists the microVMs, ordered by name.
func (c *Client) MicroVMs(ctx context.Context) ([]MicroVM, error) {
	var out struct {
		MicroVMs []MicroVM `json:"microvms"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/microvms", nil, &out)
	return out.MicroVMs, err
}

// StopMicroVM stops the microVM, giving its guest r's grace, and returns it as it is after.
func (c *Client) StopMicroVM(ctx context.Context, name string, r StopRequest) (MicroVM, error) {
	var m MicroVM
	err := c.do(ctx, http.MethodPost, microVMPath(name)+"/stop", r, &m)
	return m, err
}

// StartMicroVM starts the microVM and returns it as it is after.
func (c *Client) StartMicroVM(ctx context.Context, name string) (MicroVM, error) {
	var m MicroVM
	err := c.do(ctx, http.MethodPost, microVMPath(name)+"/start", nil, &m)
	return m, err
}

// DeleteMicroVM stops and deletes the microVM and the disks made with it.
func (c *Client) DeleteMicroVM(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, microVMPath(name), nil, nil)
}

// RenewFence sets the deadline of the microVM's fence and returns the fence as it is after.
func (c *Client) RenewFence(ctx context.Context, name string, r FenceRequest) (Fence, error) {
	var f Fence
	err := c.do(ctx, http.MethodPost, microVMPath(name)+"/fence", r, &f)
	return f, err
}

// Fence returns the state of the microVM's fence.
func (c *Client) Fence(ctx context.Context, name string) (Fence, error) {
	var f Fence
	err := c.do(ctx, http.MethodGet, microVMPath(name)+"/fence", nil, &f)
	return f, err
}

// SnapshotMicroVM takes a snapshot of the running microVM, named by r.
func (c *Client) SnapshotMicroVM(ctx context.Context, name string, r SnapshotRequest) (Snapshot, error) {
	var s Snapshot
	err := c.do(ctx, http.MethodPost, microVMPath(name)+"/snapshot", r, &s)
	return s, err
}

// Snapshots lists the snapshots the host holds, ordered by name.
func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	var out struct {
		Snapshots []Snapshot `json:"snapshots"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/snapshots", nil, &out)
	return out.Snapshots, err
}

// Snapshot returns the snapshot named name.
func (c *Client) Snapshot(ctx context.Context, name string) (Snapshot, error) {
	var s Snapshot
	err := c.do(ctx, http.MethodGet, snapshotPath(name), nil, &s)
	return s, err
}

// DeleteSnapshot deletes the snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, snapshotPath(name), nil, nil)
}

// SnapshotDisk copies the microVM's disk into a disk snapshot named by r.
func (c *Client) SnapshotDisk(ctx context.Context, microVM, disk string, r DiskSnapshotRequest) (DiskSnapshot, error) {
	var d DiskSnapshot
	err := c.do(ctx, http.MethodPost, microVMPath(microVM)+"/disks/"+url.PathEscape(disk)+"/snapshot", r, &d)
	return d, err
}

// DiskSnapshots lists the disk snapshots the host keeps, ordered by name.
func (c *Client) DiskSnapshots(ctx context.Context) ([]DiskSnapshot, error) {
	var out struct {
		DiskSnapshots []DiskSnapshot `json:"diskSnapshots"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/disk-snapshots", nil, &out)
	return out.DiskSnapshots, err
}

// DeleteDiskSnapshot deletes the disk snapshot; an absent one is success.
func (c *Client) DeleteDiskSnapshot(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/disk-snapshots/"+url.PathEscape(name), nil, nil)
}

// ResumeSnapshot makes a running microVM from the snapshot.
func (c *Client) ResumeSnapshot(ctx context.Context, name string, r ResumeRequest) (MicroVM, error) {
	var m MicroVM
	err := c.do(ctx, http.MethodPost, snapshotPath(name)+"/resume", r, &m)
	return m, err
}

// Exec runs r.Command in the running microVM and returns its result, whole.
func (c *Client) Exec(ctx context.Context, microVM string, r ExecRequest) (ExecResult, error) {
	var out ExecResult
	err := c.do(ctx, http.MethodPost, microVMPath(microVM)+"/exec", r, &out)
	return out, err
}

// ReadFile returns the content of the file at guestPath in the running microVM.
func (c *Client) ReadFile(ctx context.Context, microVM, guestPath string) ([]byte, error) {
	_, data, err := c.fetch(ctx, http.MethodGet, filesPath(microVM, guestPath), nil, "")
	return data, err
}

// WriteFile replaces the file at guestPath in the running microVM with content, whole.
func (c *Client) WriteFile(ctx context.Context, microVM, guestPath string, content []byte) error {
	_, _, err := c.fetch(ctx, http.MethodPut, filesPath(microVM, guestPath), bytes.NewReader(content), "application/octet-stream")
	return err
}

func filesPath(microVM, guestPath string) string {
	return microVMPath(microVM) + "/files?" + url.Values{"path": {guestPath}}.Encode()
}
