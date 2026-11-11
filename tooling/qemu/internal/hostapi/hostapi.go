// Package hostapi is the test host's client of gaon's resource API: plain HTTPS with the test host's
// client certificate, reaching the API server through the host forward of the control-plane Node
// (ADR 0012). It decodes only the fields the tests read, so it adds no dependency on gaon's
// modules; the paths and fields are those of gaon's docs/architecture/resource-api.md at the commit
// image/gaon/pin.env names.
package hostapi

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
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/pki"
)

// Root is the group and version root of the resource API, appended to the server's address.
const Root = "/apis/compute.gaon.garam.sh/v1alpha1"

// ErrNotFound is the error of a read of a resource the API server does not have.
var ErrNotFound = errors.New("not found")

// Client calls the resource API of one cluster.
type Client struct {
	base string
	http *http.Client
}

// New returns a client of the API server at addr, "127.0.0.1:port", that trusts the cluster's CA
// and presents the test host's client certificate. The serving certificate must name addr's host.
func New(addr string, certs *pki.Cluster) (*Client, error) {
	pair, err := tls.X509KeyPair(certs.Client.CertPEM, certs.Client.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("the test host's client certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certs.CA) {
		return nil, errors.New("the CA holds no certificate")
	}
	return &Client{
		base: "https://" + addr + Root,
		http: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}},
				DisableKeepAlives: true,
			},
		},
	}, nil
}

// Condition is one condition of a Node's status.
type Condition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// Node is the part of a Node the tests read.
type Node struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Admitted bool `json:"admitted"`
	} `json:"spec"`
	Status struct {
		State      string      `json:"state"`
		Conditions []Condition `json:"conditions"`
	} `json:"status"`
}

// Ready reports whether the Node's Ready condition is True.
func (n Node) Ready() bool {
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

// Lease is the part of a Lease the tests read.
type Lease struct {
	Spec struct {
		Holder    string    `json:"holder"`
		RenewTime time.Time `json:"renewTime"`
	} `json:"spec"`
}

// Node reads the Node of that name.
func (c *Client) Node(ctx context.Context, name string) (Node, error) {
	var n Node
	return n, c.do(ctx, http.MethodGet, "/nodes/"+name, nil, &n)
}

// Lease reads the Lease of the Node of that name.
func (c *Client) Lease(ctx context.Context, name string) (Lease, error) {
	var l Lease
	return l, c.do(ctx, http.MethodGet, "/leases/"+name, nil, &l)
}

// CreateNode creates a Node of that name, admitted, as a user does, and returns an error that
// wraps nothing but the status when the API server refuses it.
func (c *Client) CreateNode(ctx context.Context, name string) error {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "compute.gaon.garam.sh/v1alpha1", "kind": "Node",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"admitted": true},
	})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPut, "/nodes/"+name, body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(data))
	case out == nil:
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s %s: decode the answer: %w", method, path, err)
	}
	return nil
}
