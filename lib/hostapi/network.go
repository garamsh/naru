package hostapi

// NetworkRequest is the body of PUT /v1/networks/{name}: the prefix of the network's routed space,
// and the address of its API endpoint, which the management plane reaches one way. The endpoint is
// optional and, when given, is inside the prefix. RemoteMembers are the addresses of the network's
// microVMs on other hosts, each in CIDR form (/32); the request replaces the list.
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
// private key is made by the host and never leaves it. MeshAddress is the host's own address on the
// tunnel, empty until orch declares one (ADR 0113 item 1).
type Tunnel struct {
	PublicKey       string   `json:"publicKey"`
	ListenPort      int      `json:"listenPort"`
	MachinePrefixes []string `json:"machinePrefixes"`
	MeshAddress     string   `json:"meshAddress,omitempty"`
}

// Peer is another host the tunnel reaches. Prefixes are the ones that host holds, and MeshAddress
// is its own address on the tunnel: together the only source addresses the tunnel accepts from it
// and the only destinations it sends to it.
type Peer struct {
	Name        string   `json:"name"`
	PublicKey   string   `json:"publicKey"`
	Endpoint    string   `json:"endpoint,omitempty"`
	Prefixes    []string `json:"prefixes"`
	MeshAddress string   `json:"meshAddress,omitempty"`
}

// PeersRequest is the body of PUT /v1/peers, and what GET /v1/peers answers: the whole list of
// peers, which replaces the host's, and the host's own mesh address, which the host puts on its
// tunnel interface. An empty MeshAddress leaves the interface with none.
type PeersRequest struct {
	MeshAddress string `json:"meshAddress,omitempty"`
	Peers       []Peer `json:"peers"`
}
