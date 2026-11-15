package layout

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/layout/pki"
)

// Where a host that joined orch keeps what it made, and what its configuration says in place of
// the enrollment key it no longer has.
const (
	// HostKeyFile is the key naru-hostd made on the data disk at its first start. The certificate orch
	// issued for it is a file of spec.files; the key is never copied.
	HostKeyFile = DataMount + "/hostd/host.key"
	// JoinedEnrollKey is what spec.host.orch.enrollKey says in a joined host's configuration. init
	// requires the field, but orch never held the key the host joined with, only its hash, and a host
	// that holds its certificate presents the certificate, not a key (ADR 0059).
	JoinedEnrollKey = "joined"
	// MachinePrefixBits is the length of the prefix orch gives each host for its Machines.
	MachinePrefixBits = 24
)

// HostProfile is what every joined host shares: the fleet-wide inputs of a host Node, which orch
// keeps and builds each host's configuration from when an operator admits it (ADR 0059). It carries
// nothing that is one host's own: no certificate, token, authority key or address. A YAML document
// of this type is what naructl gen host-profile writes and orch stores.
type HostProfile struct {
	Guest             Manifest      `yaml:"guest"`
	FirecrackerSHA256 string        `yaml:"firecrackerSHA256"`
	TimeServers       []string      `yaml:"timeServers,omitempty"`
	Registries        []Registry    `yaml:"registries,omitempty"`
	AWS               *AWS          `yaml:"aws,omitempty"`
	MetricsListen     string        `yaml:"metricsListen,omitempty"`
	Network           *HostNetwork  `yaml:"network,omitempty"`
	Storage           []StorageDisk `yaml:"storage,omitempty"`
	Extensions        []Extension   `yaml:"extensions,omitempty"`
	Orch              ProfileOrch   `yaml:"orch"`
	APIClientCA       string        `yaml:"apiClientCA,omitempty"`
	MachinePool       netip.Prefix  `yaml:"machinePool"`
}

// ProfileOrch is where a joined host's agent reaches orch after admission: spec.host.orch's url and
// caSHA256.
type ProfileOrch struct {
	URL      string `yaml:"url"`
	CASHA256 string `yaml:"caSHA256"`
}

// JoinedHost is what is one admitted host's own: the ID it enrolled under, the address it reported,
// the attributes an operator gave it, the prefix orch allocated for its Machines, the certificate
// orch's signer issued for its key, and the PEM of the authority orch's certificate chains to.
type JoinedHost struct {
	ID            string
	Address       netip.Addr
	Region        string
	Labels        map[string]string
	MachinePrefix netip.Prefix
	Certificate   string
	OrchCA        string
}

// ParseHostProfile reads a profile and checks it. A field the type does not have is refused, which
// is how a profile that carries a bootstrap token or an authority key is.
func ParseHostProfile(b []byte) (HostProfile, error) {
	var p HostProfile
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		if errors.Is(err, io.EOF) {
			return p, errors.New("the host profile is empty")
		}
		return p, fmt.Errorf("the host profile: %w", err)
	}
	return p, p.Check()
}

// Marshal is the profile as YAML.
func (p HostProfile) Marshal() ([]byte, error) { return yaml.Marshal(p) }

// HasCredentials reports whether the profile carries a registry's username and password, which a
// file of it must then be kept 0600.
func (p HostProfile) HasCredentials() bool {
	for _, r := range p.Registries {
		if r.Username != "" || r.Password != "" {
			return true
		}
	}
	return false
}

// Check reports the first thing wrong with the profile: a pool that is not an IPv4 prefix of at most
// /24 in its canonical form, a per-host secret (a private key in a PEM field), and anything the
// configuration it builds would be refused for. The last is asked of a host the profile is tried on.
func (p HostProfile) Check() error {
	pool := p.MachinePool
	switch {
	case !pool.IsValid() || !pool.Addr().Is4():
		return errors.New("the Machine pool is not an IPv4 prefix")
	case pool != pool.Masked():
		return fmt.Errorf("the Machine pool %s has host bits set; write %s", pool, pool.Masked())
	case pool.Bits() > MachinePrefixBits:
		return fmt.Errorf("the Machine pool %s is smaller than one /%d, the prefix each host is given", pool, MachinePrefixBits)
	case pool.Bits() == 0:
		return errors.New("the Machine pool is the default route")
	}
	for what, pem := range map[string]string{"apiClientCA": p.APIClientCA} {
		if strings.Contains(pem, "PRIVATE KEY") {
			return fmt.Errorf("%s holds a private key: a host profile carries nothing that is one host's own", what)
		}
	}
	for _, r := range p.Registries {
		if strings.Contains(r.CA, "PRIVATE KEY") {
			return fmt.Errorf("the CA of registry %s holds a private key: a host profile carries nothing that is one host's own", r.Host)
		}
	}
	if p.Orch.URL == "" || p.Orch.CASHA256 == "" {
		return errors.New("the profile needs orch's URL and the SHA-256 of its authority, which a joined host's agent keeps reporting to")
	}
	probe, err := probeHost(p.MachinePool)
	if err != nil {
		return err
	}
	_, err = p.Node(probe)
	return err
}

// probeHost is a host the profile is tried on: a certificate from a throwaway authority, and the
// first prefix of the pool.
func probeHost(pool netip.Prefix) (JoinedHost, error) {
	ca, err := pki.New("probe")
	if err != nil {
		return JoinedHost{}, err
	}
	pair, err := ca.Server("h-0000000000000000", net.ParseIP("192.0.2.10"))
	if err != nil {
		return JoinedHost{}, err
	}
	first := netip.PrefixFrom(pool.Addr(), MachinePrefixBits)
	return JoinedHost{
		ID: "h-0000000000000000", Address: netip.MustParseAddr("192.0.2.10"), Region: "probe",
		MachinePrefix: first, Certificate: string(pair.CertPEM), OrchCA: string(ca.CertPEM),
	}, nil
}

// Node is the Node of host h under the profile: the host API served from the certificate orch issued
// for the key the host made, naru-hostd's clients trusted through the authority orch's certificate
// chains to, and spec.host.orch so its agent keeps reporting. It runs no gaon and so carries no
// cluster authority and no token. Its UserData is the host's naru.yaml.
func (p HostProfile) Node(h JoinedHost) (Node, error) {
	if !h.Address.IsValid() || !h.Address.Is4() {
		return Node{}, errors.New("the host's address is not an IPv4 address")
	}
	if !p.MachinePool.Contains(h.MachinePrefix.Addr()) || h.MachinePrefix.Bits() != MachinePrefixBits || h.MachinePrefix != h.MachinePrefix.Masked() {
		return Node{}, fmt.Errorf("the host's Machine prefix %s is not a /%d of the pool %s", h.MachinePrefix, MachinePrefixBits, p.MachinePool)
	}
	return Node{
		Name: h.ID, Mode: HostOnly,
		Address:       netip.PrefixFrom(h.Address, 32),
		MachinePrefix: h.MachinePrefix,
		Guest:         p.Guest, FirecrackerSHA256: p.FirecrackerSHA256,
		TimeServers: p.TimeServers, Registries: p.Registries, AWS: p.AWS, MetricsListen: p.MetricsListen,
		HostNetwork: p.Network, Storage: p.Storage, Extensions: p.Extensions,
		Host: &Host{
			Region: h.Region, Labels: h.Labels, Certificate: h.Certificate, KeyFile: HostKeyFile,
			ClientCA: h.OrchCA, APIClientCA: p.APIClientCA,
		},
		HostOrch: &HostOrch{URL: p.Orch.URL, CASHA256: p.Orch.CASHA256, EnrollKey: JoinedEnrollKey},
	}, nil
}
