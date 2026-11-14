package hostapi

// NetworkRequest is the body of PUT /v1/networks/{name}: the one prefix the network lives in, and
// the address of its API endpoint, which the management plane reaches one way. The endpoint is
// optional and, when given, is inside the prefix.
type NetworkRequest struct {
	Prefix      string `json:"prefix"`
	APIEndpoint string `json:"apiEndpoint,omitempty"`
}

// Network is a virtual network of the host, closed by default: it reaches its own prefix and its
// egress, and nothing else.
type Network struct {
	Name        string `json:"name"`
	Prefix      string `json:"prefix"`
	APIEndpoint string `json:"apiEndpoint,omitempty"`
}

// Peer is another host the tunnel reaches. Prefixes are the ones that host holds: the only source
// addresses the tunnel accepts from it and the only destinations it sends to it.
type Peer struct {
	Name      string   `json:"name"`
	PublicKey string   `json:"publicKey"`
	Endpoint  string   `json:"endpoint,omitempty"`
	Prefixes  []string `json:"prefixes"`
}

// PeersRequest is the body of PUT /v1/peers, and what GET /v1/peers answers: the whole list of
// peers, which replaces the host's.
type PeersRequest struct {
	Peers []Peer `json:"peers"`
}
