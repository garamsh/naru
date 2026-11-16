package layout

import (
	"crypto/x509"
	"fmt"
	"net"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/lib/wire"
)

// Where naru-hostd's files are (docs/architecture/host.md).
const (
	// HostDir is the directory init writes naru-hostd's serving pair and client CA to.
	HostDir = "/run/naru-hostd"
	// HostService is naru-hostd's name in spec.services, and HostBinary its executable in the image.
	HostService = "naru-hostd"
	HostBinary  = "/bin/naru-hostd"
)

// Host is what makes a Node serve the host API: the host authority, which signs the serving pair
// and, kept by the operator, the clients' certificates, and the region and labels /v1/host reports.
// The authority's key is read to issue the serving pair and never carried.
type Host struct {
	// CA is the host authority. It is nil on a Node whose host API certificate was issued to a key the
	// host made (Certificate), which orch's signer holds the authority's key for.
	CA     *pki.CA
	Region string
	Labels map[string]string
	// Certificate is that certificate, PEM, and KeyFile the absolute path of the key the host made: the
	// key is never carried. Both or neither; with them naru-hostd serves from them and trusts ClientCA
	// alone for its clients.
	Certificate string
	KeyFile     string
	// APIClientCA, with Certificate, is the PEM of the authority the Node API's clients' certificates
	// chain to: spec.api serves with Certificate and KeyFile.
	APIClientCA string
	// ClientCA is the PEM of the authority orch's certificate chains to, which naru-hostd trusts
	// for its clients beside the host authority; empty trusts the host authority alone.
	ClientCA string
	// RevocationPublicKey is the PKIX PEM of the key that signs the revocation list, and
	// RevocationList the signed list orch held when it admitted the host: a joined host checks its
	// clients' certificates against the list and starts with this one (ADR 0062). Both empty on a
	// host that is not joined; a Node that runs orch takes the key from its Orch.
	RevocationPublicKey string
	RevocationList      string
}

// revocationKey is the PEM of the key that signs the list this Node checks its clients against, and
// empty when the Node checks none: a joined host's own, or the key of the orch the Node runs, whose
// signer it then reads the list from.
func (n Node) revocationKey() string {
	switch {
	case n.Host != nil && n.Host.RevocationPublicKey != "":
		return n.Host.RevocationPublicKey
	case n.Orch != nil && n.Signer != nil:
		return n.Orch.RevocationPublicKey
	}
	return ""
}

var (
	labelNameRE  = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	labelValueRE = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// check reports the first thing init would refuse in spec.host, which would stop the Node booting.
func (h Host) check() error {
	if issued := h.Certificate != "" || h.KeyFile != ""; issued {
		switch {
		case h.CA != nil:
			return fmt.Errorf("a host with an issued certificate has no host authority to issue from")
		case !x509.NewCertPool().AppendCertsFromPEM([]byte(h.Certificate)):
			return fmt.Errorf("the host's certificate is not a PEM certificate")
		case !path.IsAbs(h.KeyFile):
			return fmt.Errorf("the host's key file %q is not an absolute path", h.KeyFile)
		case h.ClientCA == "":
			return fmt.Errorf("a host with an issued certificate trusts only the authority orch's certificate chains to: it is not set")
		}
	} else if h.CA == nil {
		return fmt.Errorf("the host authority is not set")
	}
	if h.APIClientCA != "" && (h.Certificate == "" || !x509.NewCertPool().AppendCertsFromPEM([]byte(h.APIClientCA))) {
		return fmt.Errorf("the Node API's client authority needs a PEM certificate and a host with an issued certificate")
	}
	if !dnsLabel.MatchString(h.Region) {
		return fmt.Errorf("the host region %q is not a DNS label", h.Region)
	}
	if h.ClientCA != "" && !x509.NewCertPool().AppendCertsFromPEM([]byte(h.ClientCA)) {
		return fmt.Errorf("the authority naru-hostd trusts for orch is not a PEM certificate")
	}
	if (h.RevocationList != "") && h.RevocationPublicKey == "" {
		return fmt.Errorf("a revocation list needs the key that signed it")
	}
	if h.RevocationPublicKey != "" {
		if _, err := identity.ParseRevocationPublic([]byte(h.RevocationPublicKey)); err != nil {
			return fmt.Errorf("the host's revocation public key: %w", err)
		}
	}
	for k, v := range h.Labels {
		prefix, name, found := strings.Cut(k, "/")
		if !found {
			name = k
		}
		if !labelNameRE.MatchString(name) || found && (len(prefix) > 253 || !dnsSubdomain.MatchString(prefix)) {
			return fmt.Errorf("the host label key %q is not a Kubernetes label key", k)
		}
		if !labelValueRE.MatchString(v) {
			return fmt.Errorf("the host label value %q of %q is not a Kubernetes label value", v, k)
		}
	}
	return nil
}

// hostSpec is spec.host.
type hostSpec struct {
	Region string            `yaml:"region"`
	Labels map[string]string `yaml:"labels,omitempty"`
	// Network is spec.host.network.
	Network *hostNetworkSpec `yaml:"network,omitempty"`
	// Orch is spec.host.orch.
	Orch *hostOrchSpec `yaml:"orch,omitempty"`
	// RevocationPublicKey and RevocationList are spec.host.revocationPublicKey and revocationList.
	RevocationPublicKey string `yaml:"revocationPublicKey,omitempty"`
	RevocationList      string `yaml:"revocationList,omitempty"`
}

// servingIPs are the addresses the Node's serving certificates name: its own, its serving
// addresses and the loopback.
func (n Node) servingIPs() []net.IP {
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
	return ips
}

// hostFiles are naru-hostd's serving pair, issued by the host authority, and the authority's
// certificate that its clients' certificates are checked against.
func (n Node) hostFiles() ([]file, error) {
	if n.Host.Certificate != "" {
		return []file{
			{Path: path.Join(HostDir, "tls.crt"), Mode: "0644", Content: n.Host.Certificate},
			{Path: path.Join(HostDir, "client-ca.crt"), Mode: "0644", Content: n.Host.ClientCA},
		}, nil
	}
	pair, err := n.Host.CA.Server(n.Name, n.servingIPs()...)
	if err != nil {
		return nil, fmt.Errorf("issue naru-hostd's serving pair: %w", err)
	}
	// The clients are checked against a bundle: the host authority's certificate, then orch's
	// authority when orch reaches this host with its own pair.
	clientCA := string(n.Host.CA.CertPEM)
	if n.Host.ClientCA != "" {
		clientCA = strings.TrimSuffix(clientCA, "\n") + "\n" + n.Host.ClientCA
	}
	return []file{
		{Path: path.Join(HostDir, "tls.crt"), Mode: "0644", Content: string(pair.CertPEM)},
		{Path: path.Join(HostDir, "tls.key"), Mode: "0600", Content: string(pair.KeyPEM)},
		{Path: path.Join(HostDir, "client-ca.crt"), Mode: "0644", Content: clientCA},
	}, nil
}

// hostService is naru-hostd, which dials mvmd's socket and runs nft from the image's /bin.
func (n Node) hostService() service {
	keyFile := path.Join(HostDir, "tls.key")
	if n.Host.KeyFile != "" {
		keyFile = n.Host.KeyFile
	}
	return service{
		Name: HostService, Binary: HostBinary, Env: map[string]string{"PATH": "/bin"},
		Args: []string{
			"--listen", "0.0.0.0:" + wire.HostAPIPort,
			// A Node's kernel hostname is "(none)", so the host enrolls under the Node's name.
			"--host-id", n.Name,
			// Orch calls the host at the address its enrollment recorded, which is the Node's, not --listen's.
			"--advertise", net.JoinHostPort(n.Address.Addr().String(), wire.HostAPIPort),
			"--cert", path.Join(HostDir, "tls.crt"), "--key", keyFile,
			"--client-ca", path.Join(HostDir, "client-ca.crt"),
			"--host-file", path.Join(HostDir, "host.json"), "--mvmd-socket", MvmdSocket,
			// The first address of the Machines' prefix, which mvmd gives its Machines as gateway.
			"--machine-gateway", n.MachinePrefix.Masked().Addr().Next().String(),
		},
	}
}
