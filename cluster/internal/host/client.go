// Package host keeps NaruHost objects current: it asks each host's API what the host is and writes
// the answer to the object's status. The wire format is the host API's (ADR 0028), replicated here
// because this unit imports no other unit.
package host

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// ErrRejected means the host API answered and refused the client pair's role.
var ErrRejected = errors.New("the host API refused the client certificate's role")

// ErrClientPair means the client pair or the CA cannot be used to connect.
var ErrClientPair = errors.New("the client pair or the CA is not usable")

// Resources are the amounts the host API reports as capacity and as free. Memory and Disk are bytes.
type Resources struct {
	CPU    uint32 `json:"cpu"`
	Memory uint64 `json:"memory"`
	Disk   uint64 `json:"disk"`
}

// Info is the body of GET /v1/host. The fields this unit does not read are not decoded.
type Info struct {
	Region   string            `json:"region"`
	Labels   map[string]string `json:"labels"`
	Capacity Resources         `json:"capacity"`
	Free     Resources         `json:"free"`
}

// Pair is the PEM of a client pair and of the authority that signed the host's serving certificate.
type Pair struct {
	Cert, Key, CA []byte
}

// maxBody bounds what is read of an answer.
const maxBody = 1 << 20

// Fetch asks the host API at address (host:port) for GET /v1/host over mutual TLS 1.3.
func Fetch(ctx context.Context, address string, pair Pair) (Info, error) {
	cert, err := tls.X509KeyPair(pair.Cert, pair.Key)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrClientPair, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pair.CA) {
		return Info{}, fmt.Errorf("%w: ca.crt holds no certificate", ErrClientPair)
	}
	serverName, _, err := net.SplitHostPort(address)
	if err != nil {
		return Info{}, fmt.Errorf("address %q is not host:port: %w", address, err)
	}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{cert},
				RootCAs:      pool,
				ServerName:   serverName,
			},
			DisableKeepAlives: true,
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+address+"/v1/host", nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return Info{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Info{}, err
	}
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return Info{}, ErrRejected
	case resp.StatusCode != http.StatusOK:
		return Info{}, fmt.Errorf("the host API answered %s", resp.Status)
	}
	var info Info
	if err := json.Unmarshal(body, &info); err != nil {
		return Info{}, fmt.Errorf("the host API's answer is not the host document: %w", err)
	}
	return info, nil
}
