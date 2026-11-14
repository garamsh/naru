package orchapi

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxBody bounds what the client reads from an answer.
const maxBody = 64 << 20

// Client calls orch's API.
type Client struct {
	base *url.URL
	http *http.Client
}

// New returns a client of the orch at base, an https URL, that sends requests through hc.
func New(base string, hc *http.Client) (*Client, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("orch URL %q is not an https URL", base)
	}
	return &Client{base: u, http: hc}, nil
}

// HTTPClient returns an HTTP client with a timeout that dials with conf.
func HTTPClient(conf *tls.Config, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{TLSClientConfig: conf}}
}

// TrustCA returns a TLS config that trusts the CAs in pool and presents cert when it is not nil.
func TrustCA(pool *x509.CertPool, cert *tls.Certificate) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool}
	if cert != nil {
		c.Certificates = []tls.Certificate{*cert}
	}
	return c
}

// PinCA returns a TLS config that trusts orch when the chain it presents holds a certificate whose
// SHA-256 is caSHA256 (hex) and the leaf verifies against that certificate and names host, a DNS
// name or an IP address, which is the host of orch's URL. The host is checked by name because every
// certificate the authority issues carries the server EKU, so the chain alone would also accept a
// host's own certificate. It presents cert when it is not nil. The built-in verification is
// replaced by that check.
func PinCA(caSHA256, host string, cert *tls.Certificate) (*tls.Config, error) {
	want, err := hex.DecodeString(strings.ReplaceAll(strings.ToLower(caSHA256), ":", ""))
	if err != nil || len(want) != sha256.Size {
		return nil, errors.New("the CA fingerprint is not 64 hex digits")
	}
	c := &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}
	c.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("orch presented no certificate")
		}
		pool := x509.NewCertPool()
		inter := x509.NewCertPool()
		pinned := false
		for _, p := range cs.PeerCertificates[1:] {
			inter.AddCert(p)
			if sum := sha256.Sum256(p.Raw); bytes.Equal(sum[:], want) {
				pool.AddCert(p)
				pinned = true
			}
		}
		if !pinned {
			return errors.New("orch's chain holds no certificate with the pinned fingerprint")
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
			Roots: pool, Intermediates: inter,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			return err
		}
		return cs.PeerCertificates[0].VerifyHostname(host)
	}
	if cert != nil {
		c.Certificates = []tls.Certificate{*cert}
	}
	return c, nil
}

func (c *Client) do(ctx context.Context, method, path string, headers map[string]string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.JoinPath(path).String(), body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
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
		if json.Unmarshal(raw, e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		return e
	}
	if rb, ok := out.(*[]byte); ok {
		*rb = raw
		return nil
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Hosts lists the admitted hosts.
func (c *Client) Hosts(ctx context.Context) ([]Host, error) {
	var out []Host
	return out, c.do(ctx, http.MethodGet, "/v1/hosts", nil, nil, &out)
}

// Pending lists the pending enrollments.
func (c *Client) Pending(ctx context.Context) ([]Pending, error) {
	var out []Pending
	return out, c.do(ctx, http.MethodGet, "/v1/pending", nil, nil, &out)
}

// Enroll sends an enrollment request. It needs no client certificate.
func (c *Client) Enroll(ctx context.Context, req EnrollRequest) (EnrollStatus, error) {
	var out EnrollStatus
	return out, c.do(ctx, http.MethodPost, "/v1/enroll", nil, req, &out)
}

// PollEnrollment asks for hostID's enrollment, proving it holds the host's key.
func (c *Client) PollEnrollment(ctx context.Context, hostID string, key crypto.Signer, now time.Time) (EnrollStatus, error) {
	tv, sig, err := SignProof(key, hostID, now)
	if err != nil {
		return EnrollStatus{}, err
	}
	var out EnrollStatus
	h := map[string]string{HeaderTime: tv, HeaderSignature: sig}
	return out, c.do(ctx, http.MethodGet, "/v1/enroll/"+url.PathEscape(hostID), h, nil, &out)
}

// Admit admits a pending host.
func (c *Client) Admit(ctx context.Context, req AdmitRequest) (Host, error) {
	var out Host
	return out, c.do(ctx, http.MethodPost, "/v1/admit", nil, req, &out)
}

// CreateEnrollmentKey makes an enrollment key.
func (c *Client) CreateEnrollmentKey(ctx context.Context, expiresAt time.Time) (EnrollmentKey, error) {
	var out EnrollmentKey
	return out, c.do(ctx, http.MethodPost, "/v1/enrollment-keys", nil, CreateKeyRequest{ExpiresAt: expiresAt}, &out)
}

// RevokeEnrollmentKey revokes the key with id.
func (c *Client) RevokeEnrollmentKey(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/enrollment-keys/"+url.PathEscape(id), nil, nil, nil)
}

// SetLabels replaces a host's region and labels.
func (c *Client) SetLabels(ctx context.Context, hostID string, req SetLabelsRequest) (Host, error) {
	var out Host
	return out, c.do(ctx, http.MethodPut, "/v1/hosts/"+url.PathEscape(hostID)+"/labels", nil, req, &out)
}

// Events lists the event log, oldest first.
func (c *Client) Events(ctx context.Context) ([]Event, error) {
	var out []Event
	return out, c.do(ctx, http.MethodGet, "/v1/events", nil, nil, &out)
}

// CRL returns the revocation list as the signer signed it.
func (c *Client) CRL(ctx context.Context) ([]byte, error) {
	var out []byte
	return out, c.do(ctx, http.MethodGet, "/v1/crl", nil, nil, &out)
}

// Snapshot asks for an etcd snapshot now.
func (c *Client) Snapshot(ctx context.Context) (SnapshotResult, error) {
	var out SnapshotResult
	return out, c.do(ctx, http.MethodPost, "/v1/snapshot", nil, nil, &out)
}

// Heartbeat tells orch the calling host, which the client certificate names, is alive.
func (c *Client) Heartbeat(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/heartbeat", nil, nil, nil)
}
