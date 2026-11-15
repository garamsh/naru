package layout

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
	pair, err := n.API.Server(n.Name, n.servingIPs()...)
	if err != nil {
		return nil, err
	}
	return &apiSpec{ClientCA: string(n.API.CertPEM), Certificate: string(pair.CertPEM), Key: string(pair.KeyPEM)}, nil
}
