package identity

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
)

// maxCRLBody bounds the list read from a source.
const maxCRLBody = 4 << 20

// SocketSource reads the revocation list from the signer's unix socket.
type SocketSource struct{ Path string }

var _ CRLSource = SocketSource{}

// CRL implements CRLSource with GET /v1/crl of the socket.
func (s SocketSource) CRL(ctx context.Context) ([]byte, error) {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.Path)
	}}}
	defer hc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://signer/v1/crl", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxCRLBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the signer answered %d", resp.StatusCode)
	}
	return b, nil
}
