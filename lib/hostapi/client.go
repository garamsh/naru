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

// maxResponse bounds a response body the client reads.
const maxResponse = 16 << 20

// Error is a refusal or failure the host API answered: its status, and the message and the fields
// it names. Read it with errors.As.
type Error struct {
	Status  int
	Message string
	Fields  []string
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
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
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
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode >= http.StatusBadRequest {
		var eb ErrorBody
		if json.Unmarshal(data, &eb) != nil || eb.Error == "" {
			eb.Error = http.StatusText(resp.StatusCode)
		}
		return &Error{Status: resp.StatusCode, Message: eb.Error, Fields: eb.Fields}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func microVMPath(name string) string { return "/v1/microvms/" + url.PathEscape(name) }

func snapshotPath(name string) string { return "/v1/snapshots/" + url.PathEscape(name) }

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

// StopMicroVM stops the microVM and returns it as it is after.
func (c *Client) StopMicroVM(ctx context.Context, name string) (MicroVM, error) {
	var m MicroVM
	err := c.do(ctx, http.MethodPost, microVMPath(name)+"/stop", nil, &m)
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

// ResumeSnapshot makes a running microVM from the snapshot.
func (c *Client) ResumeSnapshot(ctx context.Context, name string, r ResumeRequest) (MicroVM, error) {
	var m MicroVM
	err := c.do(ctx, http.MethodPost, snapshotPath(name)+"/resume", r, &m)
	return m, err
}
