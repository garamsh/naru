package orchapi

import (
	"context"
	"net/http"
	"net/url"
)

// AddressPoolSpec is what an operator declares for an address pool: the IPv4 ranges the LAN's
// owner gave naru, and the networks whose controller may draw from them.
type AddressPoolSpec struct {
	CIDRs    []string `json:"cidrs"`
	Networks []string `json:"networks"`
}

// AddressPool is an address pool orch keeps.
type AddressPool struct {
	Name     string   `json:"name"`
	CIDRs    []string `json:"cidrs"`
	Networks []string `json:"networks"`
}

// ExposurePort is one port of an exposed Service: the port on the address and the NodePort the
// Nodes serve it at.
type ExposurePort struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	NodePort int    `json:"nodePort"`
}

// ProxyProtocolV2 is the one value of ExposureSpec.ProxyProtocol besides the empty one.
const ProxyProtocolV2 = "v2"

// ExposureSpec is what a cloud-controller-manager asks for one Service. Network is the caller's
// own; Backends are Node addresses on it. RequestedAddress is the Service's loadBalancerIP.
type ExposureSpec struct {
	Network          string         `json:"network"`
	Pool             string         `json:"pool"`
	RequestedAddress string         `json:"requestedAddress,omitempty"`
	Ports            []ExposurePort `json:"ports"`
	Backends         []string       `json:"backends"`
	ProxyProtocol    string         `json:"proxyProtocol,omitempty"`
}

// The phases of an exposure.
const (
	ExposurePending   = "pending"
	ExposureAnnounced = "announced"
)

// Exposure is an exposure orch keeps: the spec, the address orch assigned and the host that holds
// it.
type Exposure struct {
	ID            string         `json:"id"`
	Network       string         `json:"network"`
	Pool          string         `json:"pool"`
	Address       string         `json:"address"`
	Ports         []ExposurePort `json:"ports"`
	Backends      []string       `json:"backends"`
	ProxyProtocol string         `json:"proxyProtocol,omitempty"`
	// Host is the ID of the host that holds the address; empty while no host does.
	Host  string `json:"host,omitempty"`
	Phase string `json:"phase"`
}

// ApplyAddressPool creates the pool or replaces its spec.
func (c *Client) ApplyAddressPool(ctx context.Context, name string, spec AddressPoolSpec) (AddressPool, error) {
	var out AddressPool
	return out, c.do(ctx, http.MethodPut, "/v1/address-pools/"+url.PathEscape(name), nil, spec, &out)
}

// AddressPools lists the pools.
func (c *Client) AddressPools(ctx context.Context) ([]AddressPool, error) {
	var out []AddressPool
	return out, c.do(ctx, http.MethodGet, "/v1/address-pools", nil, nil, &out)
}

// DeleteAddressPool removes a pool no exposure draws from.
func (c *Client) DeleteAddressPool(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/address-pools/"+url.PathEscape(name), nil, nil, nil)
}

// ApplyExposure creates the exposure or replaces its spec, and returns it with its address.
func (c *Client) ApplyExposure(ctx context.Context, id string, spec ExposureSpec) (Exposure, error) {
	var out Exposure
	return out, c.do(ctx, http.MethodPut, "/v1/exposures/"+url.PathEscape(id), nil, spec, &out)
}

// Exposures lists the exposures the caller may see.
func (c *Client) Exposures(ctx context.Context) ([]Exposure, error) {
	var out []Exposure
	return out, c.do(ctx, http.MethodGet, "/v1/exposures", nil, nil, &out)
}

// Exposure returns one exposure of the caller's network. An id of another network is not found, as an absent one is.
func (c *Client) Exposure(ctx context.Context, id string) (Exposure, error) {
	var out Exposure
	return out, c.do(ctx, http.MethodGet, "/v1/exposures/"+url.PathEscape(id), nil, nil, &out)
}

// DeleteExposure starts the release of the exposure's address; the address is given to another
// exposure once the host that announced it confirmed the removal or its lease ran out. It is
// idempotent: an absent exposure, and an id of another network, is not an error.
func (c *Client) DeleteExposure(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/exposures/"+url.PathEscape(id), nil, nil, nil)
}
