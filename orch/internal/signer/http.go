// Package signer is orch's client of naru-signer, which listens on a unix socket of the control
// host and answers HTTP: POST /v1/host, a caller's POST /v1/caller and POST /v1/caller-renew, and
// GET /v1/crl.
package signer

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// maxBody bounds an answer of the signer.
const maxBody = 8 << 20

// ErrInvalid is a request the signer answered 400: it is malformed, such as a certificate request
// that does not parse.
var ErrInvalid = errors.New("the signer refused a malformed request")

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
	var raw []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return c.do(ctx, method, path, raw)
}

func (c *Client) do(ctx context.Context, method, path string, in []byte) ([]byte, error) {
	var body io.Reader
	if in != nil {
		body = bytes.NewReader(in)
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBody {
		return nil, fmt.Errorf("the signer's answer is longer than %d bytes", maxBody)
	}
	msg := bytes.TrimSpace(raw)
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("%w: %s", inventory.ErrSignerRefused, msg)
	case resp.StatusCode == http.StatusBadRequest:
		return nil, fmt.Errorf("%w: %s", ErrInvalid, msg)
	case resp.StatusCode == http.StatusConflict && bytes.Contains(msg, []byte(identity.ErrCallerNotDue.Error())):
		return nil, fmt.Errorf("%w: %s", identity.ErrCallerNotDue, msg)
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
	// The signer answers the certificate as DER; the Signer interface, and the host after it, take PEM.
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: out.Certificate}), nil
}

// RevokeHost forwards an operator's signed revoke-host request, which the signer verifies.
func (c *Client) RevokeHost(ctx context.Context, signed []byte) (identity.HostSerials, error) {
	raw, err := c.do(ctx, http.MethodPost, "/v1/revoke-host", signed)
	if err != nil {
		return identity.HostSerials{}, err
	}
	var out identity.HostSerials
	if err := json.Unmarshal(raw, &out); err != nil {
		return identity.HostSerials{}, fmt.Errorf("the signer's revocation answer: %w", err)
	}
	return out, nil
}

// CRL returns the signed revocation list.
func (c *Client) CRL(ctx context.Context) ([]byte, error) {
	return c.call(ctx, http.MethodGet, "/v1/crl", nil)
}

// PushCRL gives the signer a revocation list another signer holds and returns the list the signer
// holds afterwards, which is the merge of the two when each had a serial the other lacked.
func (c *Client) PushCRL(ctx context.Context, data []byte) ([]byte, error) {
	return c.do(ctx, http.MethodPost, "/v1/crl", data)
}

// RegisterCaller spends a caller's registration token for the certificate of the key csr is the
// request of. A token the signer refuses is inventory.ErrSignerRefused.
func (c *Client) RegisterCaller(ctx context.Context, token string, csr []byte) (identity.Issued, error) {
	return c.issued(ctx, "/v1/caller-register", identity.CallerRegistration{Token: token, CSR: csr})
}

// RenewCaller passes a renewal the caller signed with its current certificate. One before its last
// 30 days is identity.ErrCallerNotDue; a revoked or expired certificate is inventory.ErrSignerRefused.
func (c *Client) RenewCaller(ctx context.Context, signed []byte) (identity.Issued, error) {
	raw, err := c.do(ctx, http.MethodPost, "/v1/caller-renew", signed)
	if err != nil {
		return identity.Issued{}, err
	}
	return decodeIssued(raw)
}

func (c *Client) issued(ctx context.Context, path string, in any) (identity.Issued, error) {
	raw, err := c.call(ctx, http.MethodPost, path, in)
	if err != nil {
		return identity.Issued{}, err
	}
	return decodeIssued(raw)
}

func decodeIssued(raw []byte) (identity.Issued, error) {
	var out identity.Issued
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Certificate) == 0 {
		return identity.Issued{}, fmt.Errorf("the signer's certificate answer is not one: %s", raw)
	}
	return out, nil
}
