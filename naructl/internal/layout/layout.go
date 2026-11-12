// Package layout is the one owner of how a gaon Node is laid out: the services it runs and their
// arguments, the certificates it carries and where, mvmd's configuration, and the paths on the
// data disk (ADR 0015). It writes a Node's NoCloud user-data (naru.yaml) and network-config.
package layout

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/pki"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Where a Node's programs and files are, and the ports it serves.
const (
	// DataLabel and DataMount are the data disk's file system label and its mount point.
	DataLabel = "NARUDATA"
	DataMount = "/var/lib/naru"
	// InputsDir is the tree on the data disk that carries the registry and its image.
	InputsDir = DataMount + "/inputs"
	// RegistryAddr is where the registry on every Node listens and mvmd pulls from.
	RegistryAddr = "127.0.0.1:5000"

	// MvmdSocket is mvmd's runtime API socket, and MvmdConfig where init writes its configuration.
	MvmdSocket = "/run/mvmd.sock"
	MvmdConfig = "/run/mvmd/config.json"
	// MkfsPath is the mkfs.ext4 the image carries (ADR 0011), which mvmd's disk.mkfs names.
	MkfsPath = "/bin/mkfs.ext4"
	// SupervisorCgroup holds the supervisors of Machines, outside mvmd's cgroup.
	SupervisorCgroup = "/sys/fs/cgroup/naru/detached"

	// GaonCertDir holds the certificate files init writes; the services name them.
	GaonCertDir = "/run/gaon"
	// AgentCertDir is where the Node Agent keeps the pair it was issued, on the data disk so that it
	// survives a reboot; BootstrapTokenFile is the Node's own token, and BootstrapTokensFile the
	// control-plane Node's list of every Node's token.
	AgentCertDir        = DataMount + "/gaon-node-agent"
	BootstrapTokenFile  = GaonCertDir + "/bootstrap-token"
	BootstrapTokensFile = GaonCertDir + "/bootstrap-tokens"
	// APIPort is the TCP port gaon-apiserver serves mutual TLS on.
	APIPort = "6443"
	// EtcdDir is etcd's data directory, on the data disk beside mvmd's state directory.
	EtcdDir = DataMount + "/etcd"
	// EtcdClientURL and EtcdPeerURL are loopback only: gaon-apiserver is the one client of etcd.
	EtcdClientURL = "http://127.0.0.1:2379"
	EtcdPeerURL   = "http://127.0.0.1:2380"
)

// NIC is a network card besides the first, with a static address and no gateway.
type NIC struct {
	MAC     string
	Address netip.Prefix
}

// Node says what one Node is configured with.
type Node struct {
	// Name is the Node's name, such as "node-1".
	Name string
	// Mode is the Node's role and how it comes by its certificates. A control-plane Mode sets
	// spec.controlPlane: the Node runs etcd and gaon's control plane.
	Mode Mode

	// Address is the Node's address and its network's prefix, MAC its card's, and Gateway,
	// Nameservers and Search what its network-config names. Search may be empty.
	Address     netip.Prefix
	MAC         string
	Gateway     netip.Addr
	Nameservers []netip.Addr
	Search      string
	// ExtraNIC, when set, is a second card.
	ExtraNIC *NIC

	// APIServer is the address of the control-plane Node, which every agent dials; the API server's
	// serving certificate names it. ServingAddresses are more addresses it names.
	APIServer        netip.Addr
	ServingAddresses []netip.Addr

	// Binaries maps a service's name to the executable that runs it, in place of the image's
	// /bin/<name>.
	Binaries map[string]string
	// Disable names services left out of the Node's configuration.
	Disable []string

	// MachinePrefix is the IPv4 prefix mvmd gives its Machines; the first host address of it is the
	// Machines' gateway and resolver.
	MachinePrefix netip.Prefix
	// Guest is guest.json, and FirecrackerSHA256 the digest of the Firecracker binary.
	Guest             Manifest
	FirecrackerSHA256 string

	// Token is the Node's bootstrap token, which its agent presents for a certificate signing
	// request. Tokens, on a control-plane Node, is every Node's token, its own included: the API
	// server's token file. Neither is set when Mode has the pairs as files.
	Token  string
	Tokens []string
	// CA is the cluster's authority. Only a control-plane Node that issues carries its key.
	CA *pki.CA
	// API is the Node API authority, separate from CA. When set, spec.api carries its certificate
	// and a serving pair it issued, and never its key.
	API *pki.CA
}

type service struct {
	Name         string            `yaml:"name"`
	Binary       string            `yaml:"binary"`
	Args         []string          `yaml:"args,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	ControlPlane bool              `yaml:"controlPlane,omitempty"`
}

type file struct {
	Path    string `yaml:"path"`
	Mode    string `yaml:"mode,omitempty"`
	Content string `yaml:"content"`
}

// pkiSpec, pkiAuthority and pkiCertificate are spec.pki as docs/architecture/config.md names it.
type pkiSpec struct {
	Authorities  []pkiAuthority   `yaml:"authorities"`
	Certificates []pkiCertificate `yaml:"certificates"`
}

type pkiAuthority struct {
	Name            string `yaml:"name"`
	Certificate     string `yaml:"certificate"`
	Key             string `yaml:"key,omitempty"`
	CertificatePath string `yaml:"certificatePath"`
	KeyPath         string `yaml:"keyPath,omitempty"`
}

type pkiCertificate struct {
	Name            string   `yaml:"name"`
	Authority       string   `yaml:"authority"`
	CertificatePath string   `yaml:"certificatePath"`
	KeyPath         string   `yaml:"keyPath"`
	CommonName      string   `yaml:"commonName"`
	Usage           string   `yaml:"usage"`
	IPAddresses     []string `yaml:"ipAddresses,omitempty"`
	NodeAddresses   bool     `yaml:"nodeAddresses,omitempty"`
	Restart         []string `yaml:"restart,omitempty"`
}

type nodeConfig struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		ControlPlane bool `yaml:"controlPlane"`
		DataDisk     struct {
			Label      string `yaml:"label"`
			MountPoint string `yaml:"mountPoint"`
		} `yaml:"dataDisk"`
		Network struct {
			Forwarding []string `yaml:"forwarding"`
		} `yaml:"network"`
		Services []service `yaml:"services"`
		Files    []file    `yaml:"files"`
		PKI      *pkiSpec  `yaml:"pki,omitempty"`
		API      *apiSpec  `yaml:"api,omitempty"`
	} `yaml:"spec"`
}

// Check reports the first thing wrong with the Node's description.
func (n Node) Check() error {
	switch {
	case n.Name == "":
		return errors.New("a Node needs a name")
	case !dnsLabel.MatchString(n.Name):
		return fmt.Errorf("the name %q is not a DNS label (lower-case letters, digits and '-', at most 63, starting and ending with a letter or digit); it is the Node's CN, its etcd name and its metadata name", n.Name)
	case !n.Address.IsValid() || !n.Address.Addr().Is4():
		return errors.New("the address is not an IPv4 address with a prefix length")
	case !n.APIServer.IsValid():
		return errors.New("the API server's address is not set")
	case !n.MachinePrefix.IsValid() || !n.MachinePrefix.Addr().Is4():
		return errors.New("the Machine prefix is not an IPv4 prefix")
	case n.CA == nil:
		return errors.New("the cluster's authority is not set")
	}
	if err := n.Mode.check(n.Token, n.Tokens, n.Disable); err != nil {
		return err
	}
	if err := n.Guest.Check(); err != nil {
		return err
	}
	for _, name := range n.Disable {
		if !Gaon.Has(name) {
			return fmt.Errorf("cannot disable %q: the services that can be left out are %s", name, strings.Join(Gaon.Names(), ", "))
		}
	}
	for name, bin := range n.Binaries {
		if !Gaon.Has(name) {
			return fmt.Errorf("cannot give %q a binary: the services that can have one are %s", name, strings.Join(Gaon.Names(), ", "))
		}
		if !path.IsAbs(bin) {
			return fmt.Errorf("the binary of %s, %q, is not an absolute path", name, bin)
		}
	}
	return nil
}

func gaonPath(name string) string { return path.Join(GaonCertDir, name) }

// binary is the executable of a service: the one Binaries names, or fallback.
func (n Node) binary(name, fallback string) string {
	if b, ok := n.Binaries[name]; ok {
		return b
	}
	return fallback
}

func (n Node) apiRoot() string {
	return "https://" + net.JoinHostPort(n.APIServer.String(), APIPort) + "/apis/compute.gaon.garam.sh/v1alpha1"
}

// UserData is the Node's naru.yaml, its NoCloud user-data. It carries mvmd's configuration as a
// file for init to write under /run; its services, in the order init starts them, are mvmd, the
// registry (the scheduler may bind a Machine to any Node, and mvmd pulls from the registry on its
// own Node), then gaon's.
func (n Node) UserData() ([]byte, error) {
	if err := n.Check(); err != nil {
		return nil, err
	}
	mvmdJSON, err := n.mvmdJSON()
	if err != nil {
		return nil, err
	}
	var c nodeConfig
	c.APIVersion = "naru.garam.sh/v1alpha1"
	c.Kind = "NodeConfig"
	c.Metadata.Name = "naru-" + n.Name
	c.Spec.ControlPlane = n.Mode.ControlPlane()
	c.Spec.DataDisk.Label = DataLabel
	c.Spec.DataDisk.MountPoint = DataMount
	c.Spec.Network.Forwarding = []string{"ipv4"}
	c.Spec.Files = []file{{Path: MvmdConfig, Mode: "0600", Content: string(mvmdJSON) + "\n"}}
	certFiles, err := n.certFiles()
	if err != nil {
		return nil, err
	}
	c.Spec.Files = append(c.Spec.Files, certFiles...)
	c.Spec.Files = append(c.Spec.Files, n.tokenFiles()...)
	if n.Mode.plan(n.Name).specPKI {
		c.Spec.PKI = n.pkiSpec()
	}
	if c.Spec.API, err = n.specAPI(); err != nil {
		return nil, fmt.Errorf("issue the Node API's serving pair: %w", err)
	}
	c.Spec.Services = n.services()
	n.withoutDisabled(&c)
	return yaml.Marshal(c)
}

// withoutDisabled drops the disabled services from the Node's list, and from the restart lists of
// spec.pki, which init refuses to resolve for a service the Node does not run.
func (n Node) withoutDisabled(c *nodeConfig) {
	if len(n.Disable) == 0 {
		return
	}
	c.Spec.Services = slices.DeleteFunc(c.Spec.Services, func(s service) bool { return slices.Contains(n.Disable, s.Name) })
	if slices.Contains(n.Disable, "registry") {
		// mvmd trusts the registry's certificate through SSL_CERT_FILE; without the registry the file is not there.
		for i := range c.Spec.Services {
			if c.Spec.Services[i].Name == "mvmd" {
				c.Spec.Services[i].Env = nil
			}
		}
	}
	if c.Spec.PKI != nil {
		for i := range c.Spec.PKI.Certificates {
			r := &c.Spec.PKI.Certificates[i].Restart
			*r = slices.DeleteFunc(*r, func(s string) bool { return slices.Contains(n.Disable, s) })
		}
	}
}

// tokenFiles are the Node's token, and the list of every token on a control-plane Node, 0600.
func (n Node) tokenFiles() []file {
	p := n.Mode.plan(n.Name)
	if !p.token {
		return nil
	}
	files := []file{{Path: BootstrapTokenFile, Mode: "0600", Content: n.Token + "\n"}}
	if p.tokenList {
		files = append(files, file{Path: BootstrapTokensFile, Mode: "0600", Content: strings.Join(n.Tokens, "\n") + "\n"})
	}
	return files
}

// certFiles are the certificate files of the Node: the CA's certificate on every Node, the agent's
// pair when the pairs are files, and, on a control-plane Node that does not issue, the pairs of the
// API server, the scheduler and the controllers. The keys are 0600 and the certificates 0644. None
// carries the CA's key.
func (n Node) certFiles() ([]file, error) {
	p := n.Mode.plan(n.Name)
	if !p.caFile {
		return nil, nil // init writes the CA and every pair from spec.pki
	}
	pair := func(name, cn string) (pki.Pair, error) {
		if name == "apiserver" {
			ips := []net.IP{net.IP(n.APIServer.AsSlice())}
			for _, a := range n.ServingAddresses {
				ips = append(ips, net.IP(a.AsSlice()))
			}
			return n.CA.Server(cn, ips...)
		}
		return n.CA.Client(cn)
	}
	files := []file{{Path: gaonPath("ca.pem"), Mode: "0644", Content: string(n.CA.CertPEM)}}
	for _, ps := range p.pairs {
		pr, err := pair(ps.name, ps.cn)
		if err != nil {
			return nil, fmt.Errorf("issue %s: %w", ps.name, err)
		}
		files = append(files,
			file{Path: gaonPath(ps.name + ".pem"), Mode: "0644", Content: string(pr.CertPEM)},
			file{Path: gaonPath(ps.name + ".key"), Mode: "0600", Content: string(pr.KeyPEM)},
		)
	}
	return files, nil
}

// pkiSpec is the control-plane Node's spec.pki: the cluster's CA with its key, written where the
// services name it (the controllers sign agents' requests with it), and the three pairs the Node's
// services take, each restarting its service when init renews it. The agent's pair is not among
// them: the agent is issued its own through the API server. The paths are those the files take otherwise, so services() is the same either way.
// The serving certificate names the Node's addresses and the loopback (nodeAddresses), APIServer and
// ServingAddresses.
func (n Node) pkiSpec() *pkiSpec {
	cert := func(name, cn, usage, service string) pkiCertificate {
		return pkiCertificate{
			Name: name, Authority: "gaon", CertificatePath: gaonPath(name + ".pem"), KeyPath: gaonPath(name + ".key"),
			CommonName: cn, Usage: usage, Restart: []string{service},
		}
	}
	apiserver := cert("apiserver", "gaon-apiserver", "server", "gaon-apiserver")
	apiserver.NodeAddresses = true
	// The API server's address is named whether or not it is one of the Node's own: an operator's
	// network-config, or an address behind a forward, would otherwise leave every agent unable to verify it.
	apiserver.IPAddresses = []string{n.APIServer.String()}
	for _, a := range n.ServingAddresses {
		if a != n.APIServer {
			apiserver.IPAddresses = append(apiserver.IPAddresses, a.String())
		}
	}
	return &pkiSpec{
		Authorities: []pkiAuthority{{
			Name: "gaon", Certificate: string(n.CA.CertPEM), Key: string(n.CA.KeyPEM), CertificatePath: gaonPath("ca.pem"), KeyPath: gaonPath("ca.key"),
		}},
		Certificates: []pkiCertificate{
			apiserver,
			cert("scheduler", "gaon-scheduler", "client", "gaon-scheduler"),
			cert("controllers", "gaon-controllers", "client", "gaon-controllers"),
		},
	}
}

// services are the Node's, in the order init starts them: mvmd; the registry; etcd, then the API
// server that dials it, then the scheduler and the controllers that dial the API server, on the
// control-plane Node (init leaves them out elsewhere); the Node Agent, which dials mvmd's socket and
// the API server, last. Each dials what is before it and retries or is restarted by init, so the
// order is a courtesy and no service depends on it for correctness (docs/architecture/image.md).
func (n Node) services() []service {
	creds := n.Mode.plan(n.Name)
	list := []service{
		{
			Name: "mvmd", Binary: "/bin/mvmd", Args: []string{"-config", MvmdConfig},
			Env: map[string]string{"SSL_CERT_FILE": path.Join(InputsDir, "registry/ca.pem")},
		},
		{
			Name: "registry", Binary: n.binary("registry", path.Join(InputsDir, "bin/naru-registry")),
			Args: []string{"-listen", RegistryAddr, "-dir", path.Join(InputsDir, "registry")},
		},
		{
			Name: "etcd", Binary: n.binary("etcd", "/bin/etcd"),
			Args: []string{
				"--name", n.Name, "--data-dir", EtcdDir,
				"--listen-client-urls", EtcdClientURL, "--advertise-client-urls", EtcdClientURL,
				"--listen-peer-urls", EtcdPeerURL, "--initial-advertise-peer-urls", EtcdPeerURL,
				"--initial-cluster", n.Name + "=" + EtcdPeerURL,
			},
		},
		{
			Name: "gaon-apiserver", Binary: n.binary("gaon-apiserver", "/bin/gaon-apiserver"),
			Args: append([]string{
				"--listen", "0.0.0.0:" + APIPort, "--etcd-endpoints", EtcdClientURL,
				"--tls-client-ca", gaonPath("ca.pem"), "--tls-cert", gaonPath("apiserver.pem"), "--tls-key", gaonPath("apiserver.key"),
			}, creds.apiserverTokens...),
		},
		{
			Name: "gaon-scheduler", Binary: n.binary("gaon-scheduler", "/bin/gaon-scheduler"),
			Args: append([]string{"--api", n.apiRoot()}, tlsClient("scheduler")...),
		},
		{
			Name: "gaon-controllers", Binary: n.binary("gaon-controllers", "/bin/gaon-controllers"),
			Args: append(append([]string{"--api", n.apiRoot()}, tlsClient("controllers")...), creds.signer...),
		},
		{
			Name: "gaon-node-agent", Binary: n.binary("gaon-node-agent", "/bin/gaon-node-agent"),
			Args: append([]string{"--api-server", n.apiRoot(), "--node-name", n.Name, "--mvmd-socket", MvmdSocket}, creds.agent...),
		},
	}
	// Which services only the control-plane Node runs is the topology's.
	for i := range list {
		list[i].ControlPlane = Gaon.OnlyControlPlane(list[i].Name)
	}
	return list
}
