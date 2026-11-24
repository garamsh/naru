package machineapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxResponse bounds a response body the client reads: a guest file or an exec's output included.
const maxResponse = 64 << 20

// ErrResponseTooLarge is the failure of a call whose answer is above 64 MiB, which the client does
// not read.
var ErrResponseTooLarge = errors.New("the answer is larger than the client reads")

// Client calls the Machine API of one orch. The caller supplies the http.Client, which carries its
// identity: a TLS configuration with the caller certificate and the CA that issued orch's.
type Client struct {
	base *url.URL
	http *http.Client
}

// New returns a client of the Machine API at base, an https URL such as "https://10.0.0.1:7600",
// that sends requests through hc.
func New(base string, hc *http.Client) (*Client, error) {
	if hc == nil {
		return nil, errors.New("the machine API client needs an http.Client")
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("machine API URL %q is not an https URL", base)
	}
	return &Client{base: u, http: hc}, nil
}

// send sends body with contentType when body is not nil, and returns the status and the body of an
// answer below 400. An answer of 400 or above is an *Error.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (int, []byte, error) {
	u := c.base.JoinPath(PathPrefix, path)
	u.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
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
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(data) > maxResponse {
		return resp.StatusCode, nil, fmt.Errorf("%w: %s %s answered more than %d bytes", ErrResponseTooLarge, method, path, maxResponse)
	}
	if resp.StatusCode >= http.StatusBadRequest {
		var eb ErrorBody
		if json.Unmarshal(data, &eb) != nil || eb.Error == "" {
			eb.Error = strings.TrimSpace(string(data))
		}
		return resp.StatusCode, nil, &Error{Status: resp.StatusCode, Message: eb.Error, Code: eb.Code}
	}
	return resp.StatusCode, data, nil
}

// call sends in as JSON when it is not nil, decodes the answer into out when it is not nil, and
// returns the status.
func (c *Client) call(ctx context.Context, method, path string, query url.Values, in, out any) (int, error) {
	var body io.Reader
	contentType := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body, contentType = bytes.NewReader(b), "application/json"
	}
	status, data, err := c.send(ctx, method, path, query, body, contentType)
	if err != nil || out == nil {
		return status, err
	}
	return status, json.Unmarshal(data, out)
}

func machinePath(name string) string { return "machines/" + url.PathEscape(name) }

// CreateMachine creates the Machine name with spec, or restores it when spec.FromSnapshot is set.
// It is idempotent by name: created is true when this call made it, false when a Machine of the
// same spec, State aside, was there; another spec is a 409 *Error.
func (c *Client) CreateMachine(ctx context.Context, name string, spec Spec) (m Machine, created bool, err error) {
	status, err := c.call(ctx, http.MethodPut, machinePath(name), nil, spec, &m)
	return m, status == http.StatusCreated, err
}

// Machine returns the caller's Machine name.
func (c *Client) Machine(ctx context.Context, name string) (Machine, error) {
	var m Machine
	_, err := c.call(ctx, http.MethodGet, machinePath(name), nil, nil, &m)
	return m, err
}

// Machines lists the caller's Machines, with the event cursor the list is current at.
func (c *Client) Machines(ctx context.Context) (MachineList, error) {
	var l MachineList
	_, err := c.call(ctx, http.MethodGet, "machines", nil, nil, &l)
	return l, err
}

// StartMachine sets the Machine's desired state to running and returns it.
func (c *Client) StartMachine(ctx context.Context, name string) (Machine, error) {
	var m Machine
	_, err := c.call(ctx, http.MethodPost, machinePath(name)+"/start", nil, nil, &m)
	return m, err
}

// StopMachine sets the Machine's desired state to stopped and returns it.
func (c *Client) StopMachine(ctx context.Context, name string, r StopRequest) (Machine, error) {
	var m Machine
	_, err := c.call(ctx, http.MethodPost, machinePath(name)+"/stop", nil, r, &m)
	return m, err
}

// DeleteMachine asks for the Machine and its volumes to be removed. The name is held until they
// are; its snapshots stay.
func (c *Client) DeleteMachine(ctx context.Context, name string) error {
	_, err := c.call(ctx, http.MethodDelete, machinePath(name), nil, nil, nil)
	return err
}

func snapshotPath(name string) string { return "snapshots/" + url.PathEscape(name) }

// PutSnapshot takes the snapshot name of r.Machine's volumes. It is idempotent by name: created is
// false when a snapshot of the same Machine was there; one of another Machine is a 409 *Error.
func (c *Client) PutSnapshot(ctx context.Context, name string, r SnapshotRequest) (s Snapshot, created bool, err error) {
	status, err := c.call(ctx, http.MethodPut, snapshotPath(name), nil, r, &s)
	return s, status == http.StatusCreated, err
}

// Snapshot returns the caller's snapshot name.
func (c *Client) Snapshot(ctx context.Context, name string) (Snapshot, error) {
	var s Snapshot
	_, err := c.call(ctx, http.MethodGet, snapshotPath(name), nil, nil, &s)
	return s, err
}

// Snapshots lists the caller's snapshots, ordered by name.
func (c *Client) Snapshots(ctx context.Context) ([]Snapshot, error) {
	var l SnapshotList
	_, err := c.call(ctx, http.MethodGet, "snapshots", nil, nil, &l)
	return l.Snapshots, err
}

// DeleteSnapshot removes the snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, name string) error {
	_, err := c.call(ctx, http.MethodDelete, snapshotPath(name), nil, nil, nil)
	return err
}

// Exec runs r.Command in the running Machine and returns its exit code and output, whole. The
// call is bounded by the http.Client's timeout as well as by r.TimeoutSeconds.
func (c *Client) Exec(ctx context.Context, name string, r ExecRequest) (ExecResult, error) {
	var out ExecResult
	_, err := c.call(ctx, http.MethodPost, machinePath(name)+"/exec", nil, r, &out)
	return out, err
}

// ReadFile returns the content of the file at guestPath in the running Machine.
func (c *Client) ReadFile(ctx context.Context, name, guestPath string) ([]byte, error) {
	_, data, err := c.send(ctx, http.MethodGet, machinePath(name)+"/files", url.Values{"path": {guestPath}}, nil, "")
	return data, err
}

// WriteFile replaces the file at guestPath in the running Machine with content, whole.
func (c *Client) WriteFile(ctx context.Context, name, guestPath string, content []byte) error {
	_, _, err := c.send(ctx, http.MethodPut, machinePath(name)+"/files", url.Values{"path": {guestPath}},
		bytes.NewReader(content), "application/octet-stream")
	return err
}

// Events returns the changes of the caller's Machines after the cursor since, the whole log when
// since is empty. A cursor older than the log keeps is a 410 *Error with CodeCursorExpired.
func (c *Client) Events(ctx context.Context, since string) (EventPage, error) {
	var p EventPage
	var q url.Values
	if since != "" {
		q = url.Values{"since": {since}}
	}
	_, err := c.call(ctx, http.MethodGet, "events", q, nil, &p)
	return p, err
}

func aliasPath(hostname string) string { return "aliases/" + url.PathEscape(hostname) }

// PutAlias publishes hostname for one of the caller's Machines. The first caller to create a
// hostname holds it: created is false when the caller held it with the same request; another
// caller's is a 409 *Error with CodeAliasTaken.
func (c *Client) PutAlias(ctx context.Context, hostname string, r AliasRequest) (a Alias, created bool, err error) {
	status, err := c.call(ctx, http.MethodPut, aliasPath(hostname), nil, r, &a)
	return a, status == http.StatusCreated, err
}

// Alias returns the caller's alias hostname.
func (c *Client) Alias(ctx context.Context, hostname string) (Alias, error) {
	var a Alias
	_, err := c.call(ctx, http.MethodGet, aliasPath(hostname), nil, nil, &a)
	return a, err
}

// DeleteAlias removes the caller's alias hostname.
func (c *Client) DeleteAlias(ctx context.Context, hostname string) error {
	_, err := c.call(ctx, http.MethodDelete, aliasPath(hostname), nil, nil, nil)
	return err
}
