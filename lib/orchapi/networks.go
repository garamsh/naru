package orchapi

import (
	"context"
	"net/http"
	"net/url"
)

// NetworkSpec is what an operator declares for a network: Prefix is its Pod space, the range the
// Pod prefixes of its members come from, and APIEndpoint is the address of the API its members
// reach orch's services at, when it has one.
type NetworkSpec struct {
	Prefix      string `json:"prefix"`
	APIEndpoint string `json:"apiEndpoint,omitempty"`
}

// Network is a network orch keeps on every admitted host.
type Network struct {
	Name        string `json:"name"`
	Prefix      string `json:"prefix"`
	APIEndpoint string `json:"apiEndpoint,omitempty"`
}

// ApplyNetwork creates the network or replaces its spec.
func (c *Client) ApplyNetwork(ctx context.Context, name string, spec NetworkSpec) (Network, error) {
	var out Network
	return out, c.do(ctx, http.MethodPut, "/v1/networks/"+url.PathEscape(name), nil, spec, &out)
}

// Networks lists the networks.
func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var out []Network
	return out, c.do(ctx, http.MethodGet, "/v1/networks", nil, nil, &out)
}

// Network returns one network.
func (c *Client) Network(ctx context.Context, name string) (Network, error) {
	var out Network
	return out, c.do(ctx, http.MethodGet, "/v1/networks/"+url.PathEscape(name), nil, nil, &out)
}

// DeleteNetwork removes a network that no group uses.
func (c *Client) DeleteNetwork(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/networks/"+url.PathEscape(name), nil, nil, nil)
}
