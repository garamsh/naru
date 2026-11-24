// Package layout is the one owner of how a Node is laid out: the services it runs and their
// arguments, the certificates it carries and where, mvmd's configuration, and the paths on the
// data disk (ADR 0015, ADR 0060). It writes a Node's NoCloud user-data (naru.yaml) and
// network-config.
package layout

import (
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/lib/wire"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Where a Node's programs and files are, and the ports it serves.
const (
	// InputsDir is the tree on the data disk that carries the registry and its image.
	InputsDir = wire.DataMount + "/inputs"
	// RegistryAddr is where the registry on every Node listens and mvmd pulls from.
	RegistryAddr = "127.0.0.1:5000"

	// MvmdSocket is mvmd's runtime API socket, and MvmdConfig where init writes its configuration.
	MvmdSocket = "/run/mvmd.sock"
	MvmdConfig = "/run/mvmd/config.json"
	// SupervisorCgroup holds the supervisors of Machines, outside mvmd's cgroup.
	SupervisorCgroup = "/sys/fs/cgroup/naru/detached"

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
	HelperPath = "/bin:/run/bin"
)

// NIC is a network card besides the first, with a static address and no gateway.
type NIC struct {
	MAC     string
	Address netip.Prefix
}

// Peer is another Node on the Node's network: its address, which is the next hop to its Machines, and
// the IPv4 prefix its mvmd gives them.
type Peer struct {
	Address       netip.Addr
	MachinePrefix netip.Prefix
}

// Node says what one Node is configured with.
type Node struct {
	// Name is the Node's name, such as "node-1".
	Name string
	// Joined is set for a host orch admitted (ADR 0059): it is named by its ID, has no inputs tree and
	// so no registry service of its own, and serves the host API from the certificate orch's signer
	// issued to the key the host made.
	Joined bool
	// NoRegistry leaves the registry service out of a Node that has an inputs tree, for an operator
	// whose Nodes pull from elsewhere (gen node --disable registry).
	NoRegistry bool

	// Address is the Node's address and its network's prefix, MAC its card's, and Gateway,
	// Nameservers and Search what its network-config names. Search may be empty.
	Address     netip.Prefix
	MAC         string
	Gateway     netip.Addr
	Nameservers []netip.Addr
	Search      string
	// ExtraNIC, when set, is a second card.
	ExtraNIC *NIC

	// ServingAddresses are addresses the Node's serving certificates name besides its own and the
	// loopback.
	ServingAddresses []netip.Addr

	// MachinePrefix is the IPv4 prefix mvmd gives its Machines; the first host address of it is the
	// Machines' gateway and resolver.
	MachinePrefix netip.Prefix
	// Peers are the other Nodes on its network. The Node routes to each one's MachinePrefix via its
	// Address (spec.network.routes), and mvmd keeps a Machine's source address for traffic to it
	// (network.no_masquerade).
	Peers []Peer
	// NoMasquerade are prefixes whose traffic from a Machine keeps its source address
	// (network.no_masquerade) with no route to them, such as the Machine prefixes of the hosts a
	// tunnel carries them to, which hostd routes through the interface it makes.
	NoMasquerade []netip.Prefix
	// FirecrackerSHA256 is the digest of the Firecracker binary. The guest kernels and initrds have no
	// field: the booted image's guest.json names them (ADR 0106).
	FirecrackerSHA256 string

	// API is the Node API authority. When set, spec.api carries its certificate and a serving pair
	// it issued, and never its key.
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
	// Extensions are spec.extensions: the system extensions init mounts before any service starts,
	// each by name and the SHA-256 of its image. Orch and the signer need "naru-orch".
	Extensions []Extension

	// TimeServers are the SNTP servers init sets the clock from, each a host or host:port, in the
	// order they are asked. With none, spec.time is left out and the Node's clock is not set.
	TimeServers []string

	// Registries are the registries mvmd pulls from besides what the image trusts: spec.registries.
	// Each one's CA is the PEM, not a path. The registry service of the Node is one of them, by
	// the host RegistryAddr, when the Node is given its CA; a Joined Node runs none.
	Registries []Registry
	// AWS is the IAM Roles Anywhere profile the credential helpers use, spec.aws. The certificate
	// and key are naru-hostd's, so a Node with AWS serves the host API.
	AWS *AWS
	// Storage are the extra disks the Node offers as XFS, found by file system or partition label.
	// The first is mounted at wire.MvmdStorageMount, where mvmd's disks, snapshots and run
	// directories go; the others under init's storage root.
	Storage []StorageDisk
}

// Registry is one entry of spec.registries. At most one of Helper and Username with Password is set.
type Registry struct {
	Host     string `yaml:"host"`
	CA       string `yaml:"ca,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	Helper   string `yaml:"helper,omitempty"`
}

// AWS is spec.aws without the paths of the pair.
type AWS struct {
	Region         string `yaml:"region"`
	TrustAnchorArn string `yaml:"trustAnchorArn"`
	ProfileArn     string `yaml:"profileArn"`
	RoleArn        string `yaml:"roleArn"`
}

// StorageDisk is an extra disk: its name, and the label, the device path or the serial number that
// finds it, exactly one.
type StorageDisk struct {
	Name   string `yaml:"name"`
	Label  string `yaml:"label,omitempty"`
	Device string `yaml:"device,omitempty"`
	Serial string `yaml:"serial,omitempty"`
}

type service struct {
	Name   string            `yaml:"name"`
	Binary string            `yaml:"binary"`
	Args   []string          `yaml:"args,omitempty"`
	Env    map[string]string `yaml:"env,omitempty"`
	// RestartPolicy is "on-failure" for a service that runs until it succeeds, and empty for one init keeps running.
	RestartPolicy string `yaml:"restartPolicy,omitempty"`
	// RestartOnChange and LogFiles are files init watches or follows for the service.
	RestartOnChange []string `yaml:"restartOnChange,omitempty"`
	LogFiles        []string `yaml:"logFiles,omitempty"`
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
	Serial     string `yaml:"serial,omitempty"`
	MountPoint string `yaml:"mountPoint,omitempty"`
}

type file struct {
	Path    string `yaml:"path"`
	Mode    string `yaml:"mode,omitempty"`
	Content string `yaml:"content"`
	// Template is what init renders Content from when it writes the file ("guest"), empty when none.
	Template string `yaml:"template,omitempty"`
	// Owner is the file's numeric owner; root's when nil.
	Owner *fileOwner `yaml:"owner,omitempty"`
	// Restart names the services init restarts when the file changes in an apply.
	Restart []string `yaml:"restart,omitempty"`
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
		DataDisk struct {
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
		API      *apiSpec  `yaml:"api,omitempty"`
		Time     *timeSpec `yaml:"time,omitempty"`
		// Registries and AWS are spec.registries and spec.aws.
		Registries []registryEntry `yaml:"registries,omitempty"`
		AWS        *awsSpec        `yaml:"aws,omitempty"`
		Storage    []storage       `yaml:"storage,omitempty"`
		Host       *hostSpec       `yaml:"host,omitempty"`
		Signer     *signerSpec     `yaml:"signer,omitempty"`
		Orch       *orchSpec       `yaml:"orch,omitempty"`
		Extensions []extensionSpec `yaml:"extensions,omitempty"`
		Metrics    *metricsSpec    `yaml:"metrics,omitempty"`
	} `yaml:"spec"`
}

// Check reports the first thing wrong with the Node's description.
func (n Node) Check() error {
	switch {
	case n.Name == "":
		return errors.New("a Node needs a name")
	case !dnsLabel.MatchString(n.Name):
		return fmt.Errorf("the name %q is not a DNS label (lower-case letters, digits and '-', at most 63, starting and ending with a letter or digit); it is the Node's CN and its metadata name", n.Name)
	case !n.Address.IsValid() || !n.Address.Addr().Is4():
		return errors.New("the address is not an IPv4 address with a prefix length")
	case !n.MachinePrefix.IsValid() || !n.MachinePrefix.Addr().Is4():
		return errors.New("the Machine prefix is not an IPv4 prefix")
	case n.AWS != nil && n.Host == nil:
		return errors.New("spec.aws takes naru-hostd's certificate and key: a Node with an IAM Roles Anywhere profile serves the host API")
	}
	if err := n.checkJoined(); err != nil {
		return err
	}
	if err := n.checkPeers(); err != nil {
		return err
	}
	if err := n.checkStorage(); err != nil {
		return err
	}
	if n.Host != nil {
		if err := n.Host.check(); err != nil {
			return err
		}
	}
	return n.checkDaemons()
}

// checkJoined reports what a Joined Node lacks or carries: it serves the host API from the
// certificate it was issued, and has no peers, whose routes it is not given.
func (n Node) checkJoined() error {
	switch {
	case !n.Joined:
		return nil
	case n.Host == nil || n.Host.Certificate == "":
		return errors.New("a joined host serves the host API from the certificate it was issued")
	case len(n.Peers) > 0:
		return errors.New("a joined host has no peers")
	}
	return nil
}

// checkStorage reports the first extra disk init would refuse: a name that is not a DNS label of at
// most 12 characters (the XFS label) or is repeated, or no label, device and serial.
func (n Node) checkStorage() error {
	seen := map[string]bool{}
	for _, d := range n.Storage {
		switch {
		case !dnsLabel.MatchString(d.Name) || len(d.Name) > 12:
			return fmt.Errorf("the storage name %q is not a DNS label of at most 12 characters", d.Name)
		case d.Label == "" && d.Device == "" && d.Serial == "":
			return fmt.Errorf("storage %q has no label, device or serial", d.Name)
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

// UserData is the Node's naru.yaml, its NoCloud user-data. It carries mvmd's configuration as a
// file for init to write under /run; its services, in the order init starts them, are mvmd,
// naru-hostd when the Node serves the host API, the registry mvmd pulls from on its own Node (a
// Joined Node runs none: its registries are its profile's), and aws-credentials with spec.aws.
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
	if n.Joined {
		// A joined host is named by its ID: the join form had no name for it (ADR 0059).
		c.Metadata.Name = n.Name
	}
	c.Spec.DataDisk.Label = wire.DataLabel
	c.Spec.DataDisk.MountPoint = wire.DataMount
	c.Spec.Network.Forwarding = []string{"ipv4"}
	for _, p := range n.Peers {
		c.Spec.Network.Routes = append(c.Spec.Network.Routes, route{To: p.MachinePrefix, Via: p.Address})
	}
	c.Spec.Files = []file{{Path: MvmdConfig, Mode: "0600", Content: string(mvmdJSON) + "\n", Template: "guest"}}
	if n.Host != nil {
		hostFiles, err := n.hostFiles()
		if err != nil {
			return nil, err
		}
		c.Spec.Files = append(c.Spec.Files, hostFiles...)
		c.Spec.Host = &hostSpec{
			Region: n.Host.Region, Labels: n.Host.Labels,
			RevocationPublicKey: n.revocationKey(), RevocationList: n.Host.RevocationList,
		}
	}
	if c.Spec.API, err = n.specAPI(); err != nil {
		return nil, fmt.Errorf("issue the Node API's serving pair: %w", err)
	}
	if len(n.TimeServers) > 0 {
		c.Spec.Time = &timeSpec{Servers: n.TimeServers}
	}
	c.Spec.Registries = n.registryEntries()
	if a := n.AWS; a != nil {
		cert, key := n.hostPair()
		c.Spec.AWS = &awsSpec{Region: a.Region, TrustAnchorArn: a.TrustAnchorArn, ProfileArn: a.ProfileArn, RoleArn: a.RoleArn, Certificate: cert, Key: key}
	}
	for i, d := range n.Storage {
		e := storage{Name: d.Name, Label: d.Label, Device: d.Device, Serial: d.Serial}
		if i == 0 {
			e.MountPoint = wire.MvmdStorageMount
		}
		c.Spec.Storage = append(c.Spec.Storage, e)
	}
	n.daemonSpecs(&c)
	c.Spec.Services = n.services()
	return yaml.Marshal(c)
}

// services are the Node's, in the order init starts them: mvmd; naru-hostd, which dials it, when
// the Node serves the host API; the registry, on a Node that has an inputs tree; aws-credentials,
// whose session mvmd's credential helper reads.
func (n Node) services() []service {
	list := []service{{
		Name: "mvmd", Binary: "/bin/mvmd", Args: []string{"-config", MvmdConfig},
		Env: n.mvmdEnv(), LogFiles: []string{EcrLoginLog},
	}}
	if n.Host != nil {
		list = append(list, n.hostService())
	}
	if !n.Joined && !n.NoRegistry {
		// A host that joined has no inputs tree, so no registry binary: it pulls from the registries
		// of its profile.
		list = append(list, service{
			Name: "registry", Binary: path.Join(InputsDir, "bin/naru-registry"),
			Args: []string{"-listen", RegistryAddr, "-dir", path.Join(InputsDir, "registry")},
		})
	}
	if a := n.AWS; a != nil {
		list = append(list, n.awsCredentials(*a))
	}
	return list
}

// awsCredentials is the service that serves the Node's IAM Roles Anywhere session on the
// loopback, as the instance metadata endpoint the AWS SDK reads (ADR 0029). naru-hostd's pair is
// its certificate and key; a new certificate restarts it.
func (n Node) awsCredentials(a AWS) service {
	cert, key := n.hostPair()
	return service{
		Name: "aws-credentials", Binary: "/bin/aws_signing_helper",
		Args: []string{
			"serve", "--certificate", cert, "--private-key", key,
			"--trust-anchor-arn", a.TrustAnchorArn, "--profile-arn", a.ProfileArn, "--role-arn", a.RoleArn,
			"--region", a.Region, "--port", AWSCredentialsPort,
		},
		RestartOnChange: []string{cert},
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
