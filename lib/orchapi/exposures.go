package orchapi

import (
	"context"
	"net/http"
	"net/url"

	"github.com/garamsh/naru/lib/wire"
)

// AddressPoolSpec is what an operator declares for an address pool: the IPv4 ranges the LAN's
// owner gave naru, and the networks whose exposures may draw from them.
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

// ExposurePort is one port of an exposure: the port on the address and the port the backends serve
// it at.
type ExposurePort struct {
	Protocol    wire.Protocol `json:"protocol"`
	Port        int           `json:"port"`
	BackendPort int           `json:"backendPort"`
}

// ProxyProtocolV2 is the one value of ExposureSpec.ProxyProtocol besides the empty one.
const ProxyProtocolV2 = "v2"

// ExposureSpec is what the operator declares for one exposure. Network is the one the exposure is
// of; Backends are members' addresses on it. RequestedAddress is the address asked for in the pool.
type ExposureSpec struct {
	Network          string         `json:"network"`
	Pool             string         `json:"pool"`
	RequestedAddress string         `json:"requestedAddress,omitempty"`
	Ports            []ExposurePort `json:"ports"`
	Backends         []string       `json:"backends"`
	ProxyProtocol    string         `json:"proxyProtocol,omitempty"`
}

// OwnerOperator is the owner of every exposure orch makes. An exposure stored before ADR 0114 may
// be owned by "exposer", and one stored before ADR 0102 by none: orch leases neither, and the
// operator deletes them.
const OwnerOperator = "operator"

// The phases of an exposure.
const (
	ExposurePending   = "pending"
	ExposureAnnounced = "announced"
)

// Exposure is an exposure orch keeps: the spec, the address orch assigned and the host that holds
// it.
type Exposure struct {
	ID      string `json:"id"`
	Network string `json:"network"`
	// Owner is OwnerOperator, the role that made it, but in an exposure stored before ADR 0114.
	Owner         string         `json:"owner"`
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

// Exposures lists every network's exposures.
func (c *Client) Exposures(ctx context.Context) ([]Exposure, error) {
	var out []Exposure
	return out, c.do(ctx, http.MethodGet, "/v1/exposures", nil, nil, &out)
}

// DeleteNetworkExposure starts the release of one exposure of a network, of whichever owner; the
// address is given to another exposure once the host that announced it confirmed the removal or
// its lease ran out. It is idempotent: an absent exposure is not an error.
func (c *Client) DeleteNetworkExposure(ctx context.Context, network, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/networks/"+url.PathEscape(network)+"/exposures/"+url.PathEscape(id), nil, nil, nil)
}

// ApplyNetworkExposure creates the operator's exposure of the network or replaces its spec, and
// returns it with its address. The exposure is owned by the operator.
func (c *Client) ApplyNetworkExposure(ctx context.Context, network, id string, spec ExposureSpec) (Exposure, error) {
	var out Exposure
	return out, c.do(ctx, http.MethodPut, "/v1/networks/"+url.PathEscape(network)+"/exposures/"+url.PathEscape(id), nil, spec, &out)
}

// NetworkExposure returns one exposure of a network, of whichever owner.
func (c *Client) NetworkExposure(ctx context.Context, network, id string) (Exposure, error) {
	var out Exposure
	return out, c.do(ctx, http.MethodGet, "/v1/networks/"+url.PathEscape(network)+"/exposures/"+url.PathEscape(id), nil, nil, &out)
}
