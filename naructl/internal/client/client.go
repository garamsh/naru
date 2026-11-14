package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client calls one Node's API.
type Client struct {
	base string
	http *http.Client
}

// Dial returns a Client for the Node at node, an IP address with or without a port; when node is
// empty it is the file's first endpoint. It speaks TLS 1.3 only, trusts the file's authority and
// presents the file's pair. Nothing is sent until a call.
func Dial(f File, node string) (*Client, error) { return DialPort(f, node, DefaultPort) }

// DialPort is Dial for a server whose port is port when node names none: the host API's.
func DialPort(f File, node, port string) (*Client, error) {
	if node == "" {
		node = f.Endpoints[0]
	}
	addr, err := AddressPort(node, port)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(f.CA)) {
		return nil, errors.New("the client file's ca is not a PEM certificate")
	}
	pair, err := tls.X509KeyPair([]byte(f.Certificate), []byte(f.Key))
	if err != nil {
		return nil, fmt.Errorf("the client file's certificate and key: %w", err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}}}
	return &Client{base: "https://" + addr, http: &http.Client{Transport: tr}}, nil
}

// StatusError is a refusal or a failure the Node answered with, which is any status but 2xx.
type StatusError struct {
	Status  int
	Message string
	// Fields name what a 422 concerns.
	Fields []string
}

func (e *StatusError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
	if len(e.Fields) > 0 {
		fmt.Fprintf(&b, " (fields: %s)", strings.Join(e.Fields, ", "))
	}
	return b.String()
}

// maxBody bounds what is read of a response that is parsed whole.
const maxBody = 16 << 20

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	return nil, statusError(resp)
}

// statusError reads the body of a response that is not 200. A body that is not the Node API's
// error shape is reported as its text, cut short.
func statusError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	var body struct {
		Error  string   `json:"error"`
		Fields []string `json:"fields"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || body.Error == "" {
		text := strings.TrimSpace(string(raw))
		if len(text) > 200 {
			text = text[:200] + "..."
		}
		return &StatusError{Status: resp.StatusCode, Message: text}
	}
	return &StatusError{Status: resp.StatusCode, Message: body.Error, Fields: body.Fields}
}

func (c *Client) getJSON(ctx context.Context, path string, into any) error {
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(into); err != nil {
		return fmt.Errorf("GET %s: the answer is not what the Node API sends: %w", path, err)
	}
	return nil
}

// Version is GET /v1/version.
type Version struct {
	Version       string  `json:"version"`
	BootID        string  `json:"bootId"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
}

// Version returns the Node's naru version, boot ID and uptime.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.getJSON(ctx, "/v1/version", &v)
	return v, err
}

// Service is one entry of GET /v1/services.
type Service struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	PID       int    `json:"pid"`
	Restarts  int    `json:"restarts"`
	StartedAt string `json:"startedAt"`
}

// Services returns each service's state.
func (c *Client) Services(ctx context.Context) ([]Service, error) {
	var body struct {
		Services []Service `json:"services"`
	}
	err := c.getJSON(ctx, "/v1/services", &body)
	return body.Services, err
}

// Logs copies the kept lines of a service to w. With follow it keeps copying each new line until
// ctx ends, which is not an error.
func (c *Client) Logs(ctx context.Context, service string, follow bool, w io.Writer) error {
	path := "/v1/services/" + url.PathEscape(service) + "/logs"
	if follow {
		path += "?follow=true"
	}
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(w, resp.Body); err != nil && ctx.Err() == nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

// Config is GET /v1/config: the revision and the document, the configuration with private material
// redacted and its status, left as the JSON the Node sent.
type Config struct {
	Revision int             `json:"revision"`
	Config   json.RawMessage `json:"config"`
}

// Config returns the Node's stored configuration.
func (c *Client) Config(ctx context.Context) (Config, error) {
	var v Config
	err := c.getJSON(ctx, "/v1/config", &v)
	return v, err
}

// Apply modes.
const (
	NoReboot = "no-reboot"
	Staged   = "staged"
	Reboot   = "reboot"
)

// Action is one thing an apply did, or with a dry run would do.
type Action struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

// Applied is the answer to POST /v1/config.
type Applied struct {
	Revision int      `json:"revision"`
	Mode     string   `json:"mode"`
	DryRun   bool     `json:"dryRun"`
	Actions  []Action `json:"actions"`
}

// Apply merge-patches the Node's configuration at revision with patch. The Node refuses a revision
// that is no longer its own with a 409 StatusError.
func (c *Client) Apply(ctx context.Context, revision int, patch map[string]any, mode string, dryRun bool) (Applied, error) {
	if mode != NoReboot && mode != Staged && mode != Reboot {
		return Applied{}, fmt.Errorf("the mode %q is not %s, %s or %s", mode, NoReboot, Staged, Reboot)
	}
	resp, err := c.do(ctx, http.MethodPost, "/v1/config", map[string]any{"revision": revision, "patch": patch, "mode": mode, "dryRun": dryRun})
	if err != nil {
		return Applied{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var a Applied
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&a); err != nil {
		return Applied{}, fmt.Errorf("POST /v1/config: the answer is not what the Node API sends: %w", err)
	}
	return a, nil
}

// Reboot asks the Node to stop its services and reboot, and returns the actions it accepted. The
// Node answers before it reboots, and its connection then ends.
func (c *Client) Reboot(ctx context.Context) ([]Action, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v1/reboot", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Actions []Action `json:"actions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return nil, fmt.Errorf("POST /v1/reboot: the answer is not what the Node API sends: %w", err)
	}
	return body.Actions, nil
}
