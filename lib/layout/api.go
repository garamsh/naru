package layout

// apiSpec is spec.api as docs/architecture/config.md names it. listen is left out: init serves on
// its default.
type apiSpec struct {
	ClientCA    string `yaml:"clientCA"`
	Certificate string `yaml:"certificate"`
	// Key is the PEM of the serving key; KeyFile is the path of a key on the Node instead, exactly one
	// of the two.
	Key     string `yaml:"key,omitempty"`
	KeyFile string `yaml:"keyFile,omitempty"`
	// RevocationPublicKey is the PEM of the key that signs the revocation list the API checks its
	// clients against (spec.api.revocationPublicKey).
	RevocationPublicKey string `yaml:"revocationPublicKey,omitempty"`
}

// specAPI is the Node's spec.api: the Node API authority's certificate as the client CA, and a
// serving pair from that authority naming the Node's address, its serving addresses and the
// loopback. The authority's key is not carried. It is nil when the Node has no API authority.
func (n Node) specAPI() (*apiSpec, error) {
	a, err := n.apiAuthority()
	if a != nil {
		a.RevocationPublicKey = n.revocationKey()
	}
	return a, err
}

func (n Node) apiAuthority() (*apiSpec, error) {
	if h := n.Host; h != nil && h.APIClientCA != "" {
		// A joined host has no API authority to issue from: it serves the Node API with the
		// certificate its host API has, whose key is the one the host made, which stays in its file.
		return &apiSpec{ClientCA: h.APIClientCA, Certificate: h.Certificate, KeyFile: h.KeyFile}, nil
	}
	if n.API == nil {
		return nil, nil
	}
	pair, err := n.API.Server(n.Name, n.servingIPs()...)
	if err != nil {
		return nil, err
	}
	return &apiSpec{ClientCA: string(n.API.CertPEM), Certificate: string(pair.CertPEM), Key: string(pair.KeyPEM)}, nil
}
