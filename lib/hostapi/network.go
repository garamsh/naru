package hostapi

// NetworkRequest is the body of PUT /v1/networks/{name}: the prefix of the network's Pod and routed
// space, and the address of its API endpoint, which the management plane reaches one way. The
// endpoint is optional and, when given, is inside the prefix. RemoteMembers are the addresses and
// pod prefixes of the network's microVMs on other hosts, each in CIDR form (/32 for an address);
// the request replaces the list.
type NetworkRequest struct {
	Prefix        string   `json:"prefix"`
	APIEndpoint   string   `json:"apiEndpoint,omitempty"`
	RemoteMembers []string `json:"remoteMembers"`
}

// Network is a virtual network of the host, closed by default: a membership set that reaches its
// own members and its egress, and nothing else.
type Network struct {
	Name          string   `json:"name"`
	Prefix        string   `json:"prefix"`
	APIEndpoint   string   `json:"apiEndpoint,omitempty"`
	RemoteMembers []string `json:"remoteMembers"`
}

// Tunnel is what GET /v1/tunnel answers: what another host needs to peer with this one. The
// private key is made by the host and never leaves it.
type Tunnel struct {
	PublicKey       string   `json:"publicKey"`
	ListenPort      int      `json:"listenPort"`
	MachinePrefixes []string `json:"machinePrefixes"`
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
