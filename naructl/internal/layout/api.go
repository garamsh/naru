package layout

import (
	"net"
	"net/netip"
	"slices"
)

// apiSpec is spec.api as docs/architecture/config.md names it. listen is left out: init serves on
// its default.
type apiSpec struct {
	ClientCA    string `yaml:"clientCA"`
	Certificate string `yaml:"certificate"`
	Key         string `yaml:"key"`
}

// specAPI is the Node's spec.api: the Node API authority's certificate as the client CA, and a
// serving pair from that authority naming the Node's address, its serving addresses and the
// loopback. The authority's key is not carried. It is nil when the Node has no API authority.
func (n Node) specAPI() (*apiSpec, error) {
	if n.API == nil {
		return nil, nil
	}
	named := []netip.Addr{n.Address.Addr()}
	for _, a := range append(slices.Clone(n.ServingAddresses), netip.MustParseAddr("127.0.0.1")) {
		if !slices.Contains(named, a) {
			named = append(named, a)
		}
	}
	var ips []net.IP
	for _, a := range named {
		ips = append(ips, net.IP(a.AsSlice()))
	}
	pair, err := n.API.Server(n.Name, ips...)
	if err != nil {
		return nil, err
	}
	return &apiSpec{ClientCA: string(n.API.CertPEM), Certificate: string(pair.CertPEM), Key: string(pair.KeyPEM)}, nil
}
