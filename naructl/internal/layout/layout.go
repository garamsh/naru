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
	// MvmdDisks is the directory mvmd keeps its disk images in, which it does not let a setting
	// move: a storage disk is mounted over it.
	MvmdDisks = DataMount + "/mvmd/disks"
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
	AgentCertDir = DataMount + "/gaon-node-agent"
	// AgentPair is the file of AgentCertDir that holds the Node Agent's certificate and key, and
	// AgentOrganization the Organization its signer requires of the certificate's subject.
	AgentPair         = "node-agent.pem"
	AgentOrganization = "gaon:nodes"
	// RegistryFilesDir is where init writes what mvmd reads for spec.registries.
	RegistryFilesDir = "/run/naru"
	// MvmdHome is mvmd's HOME, which init makes with mode 0700 under RegistryFilesDir: the
	// docker-credential-ecr-login it runs writes its log to $HOME/.ecr/log/ecr-login.log.
	MvmdHome = RegistryFilesDir + "/registry/home"
	// EcrLoginLog is that log, which init keeps as lines of mvmd so `naructl logs mvmd` shows it.
	EcrLoginLog = MvmdHome + "/.ecr/log/ecr-login.log"
	// AWSCredentialsPort is the loopback TCP port the aws-credentials service serves the instance
	// metadata endpoint on, and AWSCredentialsEndpoint what mvmd's AWS_EC2_METADATA_SERVICE_ENDPOINT
	// is (ADR 0029). 9911 is aws_signing_helper's default; no other service of a Node uses it.
	AWSCredentialsPort     = "9911"
	AWSCredentialsEndpoint = "http://127.0.0.1:" + AWSCredentialsPort
	// HelperPath is mvmd's PATH: the image's programs and /run/bin, where a file in spec.files may
	// add a credential helper.
	HelperPath          = "/bin:/run/bin"
	BootstrapTokenFile  = GaonCertDir + "/bootstrap-token"
	BootstrapTokensFile = GaonCertDir + "/bootstrap-tokens"
	// APIPort is the TCP port gaon-apiserver serves mutual TLS on.
	APIPort = "6443"
	// EtcdDir is etcd's data directory, on the data disk beside mvmd's state directory.
	EtcdDir = DataMount + "/etcd"
)

// NIC is a network card besides the first, with a static address and no gateway.
type NIC struct {
	MAC     string
	Address netip.Prefix
}

// Peer is another Node of the cluster: its address, which is the next hop to its Machines, and
// the IPv4 prefix its mvmd gives them.
type Peer struct {
	Address       netip.Addr
	MachinePrefix netip.Prefix
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
	// Members are the control-plane Nodes of a control plane of several, in the priority order the
	// holder of APIServer, then the virtual address, takes them in; the Node is one. Empty is a
	// control plane of the Node alone (ADR 0024).
	Members []Member

	// Binaries maps a service's name to the executable that runs it, in place of the image's
	// /bin/<name>.
	Binaries map[string]string
	// Disable names services left out of the Node's configuration.
	Disable []string

	// MachinePrefix is the IPv4 prefix mvmd gives its Machines; the first host address of it is the
	// Machines' gateway and resolver.
	MachinePrefix netip.Prefix
	// Peers are the other Nodes of the cluster. The Node routes to each one's MachinePrefix via its
	// Address (spec.network.routes), and mvmd keeps a Machine's source address for traffic to it
	// (network.no_masquerade).
	Peers []Peer
	// NoMasquerade are prefixes whose traffic from a Machine keeps its source address
	// (network.no_masquerade) with no route to them, such as the Machine prefixes of the hosts a
	// tunnel carries them to, which hostd routes through the interface it makes.
	NoMasquerade []netip.Prefix
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
	// Host, when set, makes the Node serve the host API: naru-hostd runs, with a serving pair
	// issued from Host's authority, and spec.host carries its region and labels.
	Host *Host
	// HostNetwork, Signer, Orch and MetricsListen are spec.host.network, spec.signer, spec.orch and
	// spec.metrics (daemons.go). Each is left out of the configuration when nil or empty.
	HostNetwork   *HostNetwork
	HostOrch      *HostOrch
	Signer        *Signer
	Orch          *Orch
	MetricsListen string

	// TimeServers are the SNTP servers init sets the clock from, each a host or host:port, in the
	// order they are asked. With none, spec.time is left out and the Node's clock is not set.
	TimeServers []string

	// Registries are the registries mvmd pulls from besides what the image trusts: spec.registries.
	// Each one's CA is the PEM, not a path. The registry service of the Node is one of them, by
	// the host RegistryAddr, when the Node is given its CA.
	Registries []Registry
	// AWS is the IAM Roles Anywhere profile the credential helpers use, spec.aws. The Node's
	// certificate and key are the Node Agent's pair, AgentCertDir/AgentPair.
	AWS *AWS
	// Storage are the extra disks the Node offers as XFS, found by file system or partition label.
	// The first is mounted where mvmd keeps its disks (MvmdDisks); the others under init's storage
	// root.
	Storage []StorageDisk

	// Kube, when set, makes the Node a Kubernetes Node (ADR 0031): its description is the Kubernetes
	// topology's, and the Machine, token and Mode fields are not used.
	Kube *KubeNode
}

// Registry is one entry of spec.registries. At most one of Helper and Username with Password is set.
type Registry struct {
	Host, CA, Username, Password, Helper string
}

// AWS is spec.aws without the paths of the pair.
type AWS struct {
	Region, TrustAnchorArn, ProfileArn, RoleArn string
}

// StorageDisk is an extra disk: its name, and the label or the device path that finds it, exactly one.
type StorageDisk struct {
	Name, Label, Device string
}

type service struct {
	Name         string            `yaml:"name"`
	Binary       string            `yaml:"binary"`
	Args         []string          `yaml:"args,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	ControlPlane bool              `yaml:"controlPlane,omitempty"`
	KeptPair     *keptPair         `yaml:"keptPair,omitempty"`
	// RestartOnChange and LogFiles are files init watches or follows for the service.
	RestartOnChange []string `yaml:"restartOnChange,omitempty"`
	LogFiles        []string `yaml:"logFiles,omitempty"`
}

// keptPair is the pair a service keeps on the data disk, which init sets aside when its
// certificate's subject lacks the Organization the service's signer requires.
type keptPair struct {
	File         string `yaml:"file"`
	Organization string `yaml:"organization"`
}

type registryEntry struct {
	Host       string      `yaml:"host"`
	CA         string      `yaml:"ca,omitempty"`
	Credential *credential `yaml:"credential,omitempty"`
}

type credential struct {
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	Helper   string `yaml:"helper,omitempty"`
}

type awsSpec struct {
	Region         string `yaml:"region"`
	TrustAnchorArn string `yaml:"trustAnchorArn"`
	ProfileArn     string `yaml:"profileArn"`
	RoleArn        string `yaml:"roleArn"`
	Certificate    string `yaml:"certificate"`
	Key            string `yaml:"key"`
}

// storage is an entry of spec.storage.
type storage struct {
	Name       string `yaml:"name"`
	Label      string `yaml:"label,omitempty"`
	Device     string `yaml:"device,omitempty"`
	MountPoint string `yaml:"mountPoint,omitempty"`
}

type file struct {
	Path    string `yaml:"path"`
	Mode    string `yaml:"mode,omitempty"`
	Content string `yaml:"content"`
	// Owner is the file's numeric owner; root's when nil.
	Owner *fileOwner `yaml:"owner,omitempty"`
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
	Organization    string   `yaml:"organization,omitempty"`
	Usage           string   `yaml:"usage"`
	DNSNames        []string `yaml:"dnsNames,omitempty"`
	IPAddresses     []string `yaml:"ipAddresses,omitempty"`
	NodeAddresses   bool     `yaml:"nodeAddresses,omitempty"`
	Restart         []string `yaml:"restart,omitempty"`
}

// timeSpec is spec.time.
type timeSpec struct {
	Servers []string `yaml:"servers"`
}

// route is an entry of spec.network.routes.
type route struct {
	To  netip.Prefix `yaml:"to"`
	Via netip.Addr   `yaml:"via"`
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
			Label       string `yaml:"label"`
			MountPoint  string `yaml:"mountPoint"`
			FormatBlank bool   `yaml:"formatBlank,omitempty"`
		} `yaml:"dataDisk"`
		Network struct {
			Forwarding []string `yaml:"forwarding"`
			Routes     []route  `yaml:"routes,omitempty"`
		} `yaml:"network"`
		Services []service `yaml:"services"`
		Files    []file    `yaml:"files"`
		PKI      *pkiSpec  `yaml:"pki,omitempty"`
		API      *apiSpec  `yaml:"api,omitempty"`
		Time     *timeSpec `yaml:"time,omitempty"`
		// Registries and AWS are spec.registries and spec.aws.
		Registries []registryEntry `yaml:"registries,omitempty"`
		AWS        *awsSpec        `yaml:"aws,omitempty"`
		Storage    []storage       `yaml:"storage,omitempty"`
		Host       *hostSpec       `yaml:"host,omitempty"`
		Signer     *signerSpec     `yaml:"signer,omitempty"`
		Orch       *orchSpec       `yaml:"orch,omitempty"`
		Metrics    *metricsSpec    `yaml:"metrics,omitempty"`
	} `yaml:"spec"`
}

// Check reports the first thing wrong with the Node's description.
func (n Node) Check() error {
	if n.Kube != nil {
		return n.checkKube()
	}
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
	if err := n.checkPeers(); err != nil {
		return err
	}
	if err := n.checkStorage(); err != nil {
		return err
	}
	if err := n.Mode.check(n.Token, n.Tokens, n.Disable); err != nil {
		return err
	}
	if err := n.checkMembers(); err != nil {
		return err
	}
	if err := n.Guest.Check(); err != nil {
		return err
	}
	if n.Host != nil {
		if err := n.Host.check(); err != nil {
			return err
		}
	}
	if err := n.checkDaemons(); err != nil {
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

// checkStorage reports the first extra disk init would refuse: a name that is not a DNS label of at
// most 12 characters (the XFS label) or is repeated, or an empty label and device.
func (n Node) checkStorage() error {
	seen := map[string]bool{}
	for _, d := range n.Storage {
		switch {
		case !dnsLabel.MatchString(d.Name) || len(d.Name) > 12:
			return fmt.Errorf("the storage name %q is not a DNS label of at most 12 characters", d.Name)
		case d.Label == "" && d.Device == "":
			return fmt.Errorf("storage %q has no label or device", d.Name)
		case seen[d.Name]:
			return fmt.Errorf("storage %q is given twice", d.Name)
		}
		seen[d.Name] = true
	}
	return nil
}

// checkPeers reports the first peer the Node cannot route to: one whose address is not an IPv4
// address on the Node's own network, is the Node's own or another peer's, or whose Machine prefix is
// not an IPv4 prefix in its canonical form, or overlaps the Node's network, its own Machine prefix
// or another peer's. A next hop must be on-link, and the prefixes must be disjoint for a route to a
// peer's Machines to lead to the right Node.
func (n Node) checkPeers() error {
	for i, p := range n.Peers {
		switch {
		case !p.Address.IsValid() || !p.Address.Is4():
			return fmt.Errorf("peer %d: the address is not an IPv4 address", i+1)
		case !n.Address.Masked().Contains(p.Address):
			return fmt.Errorf("peer %s: the address is not on the Node's network %s, so it cannot be a next hop", p.Address, n.Address.Masked())
		case p.Address == n.Address.Addr():
			return fmt.Errorf("peer %s: the address is the Node's own", p.Address)
		case !p.MachinePrefix.IsValid() || !p.MachinePrefix.Addr().Is4():
			return fmt.Errorf("peer %s: the Machine prefix is not an IPv4 prefix", p.Address)
		case p.MachinePrefix != p.MachinePrefix.Masked():
			return fmt.Errorf("peer %s: the Machine prefix %s has host bits set; write %s", p.Address, p.MachinePrefix, p.MachinePrefix.Masked())
		case p.MachinePrefix.Bits() == 0:
			return fmt.Errorf("peer %s: the Machine prefix is the default route", p.Address)
		case p.MachinePrefix.Overlaps(n.Address.Masked()):
			return fmt.Errorf("peer %s: the Machine prefix %s overlaps the Node's network %s", p.Address, p.MachinePrefix, n.Address.Masked())
		case p.MachinePrefix.Overlaps(n.MachinePrefix):
			return fmt.Errorf("peer %s: the Machine prefix %s overlaps the Node's own %s", p.Address, p.MachinePrefix, n.MachinePrefix)
		}
		for _, q := range n.Peers[:i] {
			switch {
			case q.Address == p.Address:
				return fmt.Errorf("peer %s is listed twice", p.Address)
			case q.MachinePrefix.Overlaps(p.MachinePrefix):
				return fmt.Errorf("peers %s and %s have overlapping Machine prefixes %s and %s", q.Address, p.Address, q.MachinePrefix, p.MachinePrefix)
			}
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
	if n.Kube != nil {
		return n.kubeUserData()
	}
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
	for _, p := range n.Peers {
		c.Spec.Network.Routes = append(c.Spec.Network.Routes, route{To: p.MachinePrefix, Via: p.Address})
	}
	c.Spec.Files = []file{{Path: MvmdConfig, Mode: "0600", Content: string(mvmdJSON) + "\n"}}
	certFiles, err := n.certFiles()
	if err != nil {
		return nil, err
	}
	c.Spec.Files = append(c.Spec.Files, certFiles...)
	c.Spec.Files = append(c.Spec.Files, n.tokenFiles()...)
	if n.Host != nil {
		hostFiles, err := n.hostFiles()
		if err != nil {
			return nil, err
		}
		c.Spec.Files = append(c.Spec.Files, hostFiles...)
		c.Spec.Host = &hostSpec{Region: n.Host.Region, Labels: n.Host.Labels}
	}
	if n.Mode.plan(n.Name).specPKI {
		c.Spec.PKI = n.pkiSpec()
	}
	if c.Spec.API, err = n.specAPI(); err != nil {
		return nil, fmt.Errorf("issue the Node API's serving pair: %w", err)
	}
	if len(n.TimeServers) > 0 {
		c.Spec.Time = &timeSpec{Servers: n.TimeServers}
	}
	c.Spec.Registries = n.registryEntries()
	if a := n.AWS; a != nil {
		pair := path.Join(AgentCertDir, AgentPair)
		c.Spec.AWS = &awsSpec{Region: a.Region, TrustAnchorArn: a.TrustAnchorArn, ProfileArn: a.ProfileArn, RoleArn: a.RoleArn, Certificate: pair, Key: pair}
	}
	for i, d := range n.Storage {
		e := storage{Name: d.Name, Label: d.Label, Device: d.Device}
		if i == 0 {
			e.MountPoint = MvmdDisks
		}
		c.Spec.Storage = append(c.Spec.Storage, e)
	}
	n.daemonSpecs(&c)
	c.Spec.Services = n.services()
	if n.Host != nil {
		// After mvmd, which it dials; it is no service of the topology, so no Node leaves it out by
		// --disable and only a Node given a host authority runs it.
		c.Spec.Services = slices.Insert(c.Spec.Services, 1, n.hostService())
	}
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
		switch name {
		case etcdServer.name:
			return n.CA.Server(cn, n.etcdServerIPs()...)
		case etcdPeer.name:
			return n.CA.ClientAt(cn, net.IP(n.Address.Addr().AsSlice()))
		}
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
	etcdSrv := cert(etcdServer.name, etcdServer.cn, "server", "etcd")
	etcdSrv.NodeAddresses = true
	etcdPeerCert := cert(etcdPeer.name, etcdPeer.cn, "client", "etcd")
	etcdPeerCert.NodeAddresses = true
	return &pkiSpec{
		Authorities: []pkiAuthority{{
			Name: "gaon", Certificate: string(n.CA.CertPEM), Key: string(n.CA.KeyPEM), CertificatePath: gaonPath("ca.pem"), KeyPath: gaonPath("ca.key"),
		}},
		Certificates: []pkiCertificate{
			apiserver,
			cert("scheduler", "gaon-scheduler", "client", "gaon-scheduler"),
			cert("controllers", "gaon-controllers", "client", "gaon-controllers"),
			etcdSrv, etcdPeerCert,
			cert(apiserverTo.name, apiserverTo.cn, "client", "gaon-apiserver"),
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
			Env: n.mvmdEnv(), LogFiles: []string{EcrLoginLog},
		},
		{
			Name: "registry", Binary: n.binary("registry", path.Join(InputsDir, "bin/naru-registry")),
			Args: []string{"-listen", RegistryAddr, "-dir", path.Join(InputsDir, "registry")},
		},
		{Name: "etcd", Binary: n.binary("etcd", "/bin/etcd"), Args: n.etcdArgs()},
		{
			Name: "gaon-apiserver", Binary: n.binary("gaon-apiserver", "/bin/gaon-apiserver"),
			Args: append([]string{
				"--listen", "0.0.0.0:" + APIPort, "--etcd-endpoints", n.etcdEndpoints(),
				"--etcd-ca-file", gaonPath("ca.pem"), "--etcd-cert-file", gaonPath(apiserverTo.name + ".pem"), "--etcd-key-file", gaonPath(apiserverTo.name + ".key"),
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
			// A pair issued without the Organization the signer requires is set aside, and the
			// agent asks again (ADR 0026).
			KeptPair: &keptPair{File: path.Join(AgentCertDir, AgentPair), Organization: AgentOrganization},
		},
	}
	if a := n.AWS; a != nil {
		// After the Node Agent, whose keptPair init checks before the agent starts: the helper reads
		// the pair at start, and init starts it again when the agent replaces the file.
		list = append(list, n.awsCredentials(*a))
	}
	if n.HA() {
		fence, vip := n.haServices()
		// The rules are in place before etcd listens; the holder starts once the API server it watches has.
		list = slices.Insert(list, slices.IndexFunc(list, func(s service) bool { return s.Name == "etcd" }), fence)
		list = slices.Insert(list, slices.IndexFunc(list, func(s service) bool { return s.Name == "gaon-scheduler" }), vip)
	}
	// Which services only the control-plane Node runs is the topology's.
	for i := range list {
		list[i].ControlPlane = Gaon.OnlyControlPlane(list[i].Name)
	}
	return list
}

// awsCredentials is the service that serves the Node's IAM Roles Anywhere session on the
// loopback, as the instance metadata endpoint the AWS SDK reads (ADR 0029). The Node Agent's pair
// is its certificate and key; a new pair restarts it.
func (n Node) awsCredentials(a AWS) service {
	pair := path.Join(AgentCertDir, AgentPair)
	return service{
		Name: "aws-credentials", Binary: "/bin/aws_signing_helper",
		Args: []string{
			"serve", "--certificate", pair, "--private-key", pair,
			"--trust-anchor-arn", a.TrustAnchorArn, "--profile-arn", a.ProfileArn, "--role-arn", a.RoleArn,
			"--region", a.Region, "--port", AWSCredentialsPort,
		},
		RestartOnChange: []string{pair},
	}
}

// mvmdEnv is mvmd's environment: PATH, for the credential helpers it runs; HOME, where they write
// their logs; and with spec.aws the region and the endpoint the AWS SDK reads its credential from
// (ADR 0029).
func (n Node) mvmdEnv() map[string]string {
	env := map[string]string{"PATH": HelperPath, "HOME": MvmdHome}
	if a := n.AWS; a != nil {
		env["AWS_EC2_METADATA_SERVICE_ENDPOINT"] = AWSCredentialsEndpoint
		env["AWS_REGION"] = a.Region
	}
	return env
}

// registryEntries is spec.registries.
func (n Node) registryEntries() []registryEntry {
	var out []registryEntry
	for _, r := range n.Registries {
		e := registryEntry{Host: r.Host, CA: r.CA}
		switch {
		case r.Helper != "":
			e.Credential = &credential{Helper: r.Helper}
		case r.Username != "" || r.Password != "":
			e.Credential = &credential{Username: r.Username, Password: r.Password}
		}
		out = append(out, e)
	}
	return out
}
