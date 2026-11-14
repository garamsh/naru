// Package signer is orch's client of naru-signer, which listens on a unix socket of the control
// host and answers HTTP: POST /v1/host and GET /v1/crl.
package signer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/garamsh/naru/orch/internal/inventory"
)

// maxBody bounds an answer of the signer.
const maxBody = 8 << 20

// Client calls the signer over its unix socket.
type Client struct {
	http *http.Client
}

var _ inventory.Signer = (*Client)(nil)

// New returns a client of the signer listening on the unix socket at path.
func New(path string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
	return &Client{http: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: dial}}}
}

type hostRequest struct {
	Statement []byte `json:"statement"`
	CSR       []byte `json:"csr"`
}

type certificateResponse struct {
	Certificate []byte `json:"certificate"`
}

func (c *Client) call(ctx context.Context, method, path string, in any) ([]byte, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://signer"+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s", inventory.ErrSignerRefused, bytes.TrimSpace(raw))
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("the signer answered %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	return raw, nil
}

// IssueHost asks the signer for a host certificate on an operator-signed statement.
func (c *Client) IssueHost(ctx context.Context, statement, csr []byte) ([]byte, error) {
	raw, err := c.call(ctx, http.MethodPost, "/v1/host", hostRequest{Statement: statement, CSR: csr})
	if err != nil {
		return nil, err
	}
	var out certificateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("the signer's certificate answer: %w", err)
	}
	return out.Certificate, nil
}

// CRL returns the signed revocation list.
func (c *Client) CRL(ctx context.Context) ([]byte, error) {
	return c.call(ctx, http.MethodGet, "/v1/crl", nil)
}
