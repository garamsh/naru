package hostapi

import (
	"context"
	"net/http"
	"net/url"

	"github.com/garamsh/naru/lib/wire"
)

// ExposurePort is one port of an exposure: the port on the address and the BackendPort of the
// backends it goes to.
type ExposurePort struct {
	Protocol    wire.Protocol `json:"protocol"`
	Port        int           `json:"port"`
	BackendPort int           `json:"backendPort"`
}

// ExposureRequest is the body of PUT /v1/exposures/{address}: a lease on a LAN address. The host
// holds the address, announces it and forwards its ports to Backends while the lease is renewed,
// and removes all three LeaseSeconds after the last renewal. ProxyProtocol is "v2" or empty; with
// v2 the host writes a PROXY protocol v2 header before the payload of each TCP connection and
// refuses a UDP port.
//
// With ToProxy the lease is a proxy exposure's (ADR 0112 item 8): the host redirects the ports to
// its own naru-proxy instead, 443 to its TLS listener and 80 to its redirect listener. Network,
// Backends and ProxyProtocol are then empty, and each port is TCP 443 or 80 with BackendPort equal
// to it. A host that runs no proxy refuses it with 409.
type ExposureRequest struct {
	Network       string         `json:"network"`
	Ports         []ExposurePort `json:"ports"`
	Backends      []string       `json:"backends"`
	ProxyProtocol string         `json:"proxyProtocol,omitempty"`
	ToProxy       bool           `json:"toProxy,omitempty"`
	LeaseSeconds  int            `json:"leaseSeconds"`
}

// Exposure is a lease the host holds. ExpiresAt is RFC 3339 in UTC.
type Exposure struct {
	Address string `json:"address"`
	ExposureRequest
	ExpiresAt string `json:"expiresAt"`
}

func exposurePath(address string) string { return "/v1/exposures/" + url.PathEscape(address) }

// PutExposure takes the lease on the address, or renews it, and returns what the host holds.
func (c *Client) PutExposure(ctx context.Context, address string, r ExposureRequest) (Exposure, error) {
	var e Exposure
	err := c.do(ctx, http.MethodPut, exposurePath(address), r, &e)
	return e, err
}

// Exposures lists the leases the host holds, ordered by address.
func (c *Client) Exposures(ctx context.Context) ([]Exposure, error) {
	var out struct {
		Exposures []Exposure `json:"exposures"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/exposures", nil, &out)
	return out.Exposures, err
}

// DeleteExposure removes the address, its announcement and its rules. An address the host does
// not hold is not an error.
func (c *Client) DeleteExposure(ctx context.Context, address string) error {
	return c.do(ctx, http.MethodDelete, exposurePath(address), nil, nil)
}
