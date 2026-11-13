package node

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
)

// Where a Node's programs and files are, and the ports it serves.
const (
	// MvmdSocket is mvmd's runtime API socket.
	MvmdSocket = "/run/mvmd.sock"
	// StandinPort is the TCP port every stand-in listens on, on the Node's address.
	StandinPort = "7000"

	supervisorCgroup = "/sys/fs/cgroup/naru/detached"
	// mvmdConfig is where init writes mvmd's configuration, from the Node's naru.yaml.
	mvmdConfig = "/run/mvmd/config.json"
	// DataMount is where init mounts the data disk.
	DataMount = fixture.DataMount
	// MkfsPath is the mkfs.ext4 the image carries (ADR 0011), which mvmd's disk.mkfs names.
	MkfsPath = "/bin/mkfs.ext4"
)

// Config says what one Node of a test cluster is configured with.
type Config struct {
	// Name is the Node's name, such as "node-1".
	Name string
	// Index is the Node's position, from 1, and Total the number of Nodes.
	Index, Total int
	// ControlPlane sets spec.controlPlane.
	ControlPlane bool
	// Probe adds the service that checks a Machine outlives mvmd.
	Probe bool
	// Inputs is what the data disk carries.
	Inputs fixture.Inputs
	// Network is the Node's addresses; the zero value is QEMUNetwork(Index, Total).
	Network Network
	// HostPort, when not 0, gives the Node a second NIC on QEMU's user-mode network, which forwards
	// the host's loopback port HostPort to the API server (see hostnic.go).
	HostPort int
	// APIHostPort, when not 0 and with HostPort, adds a forward of the host's loopback port to the
	// Node API's port on the same NIC.
	APIHostPort int
	// HostReach, with HostPort, leaves the second NIC's user-mode network unrestricted, so the
	// guest reaches the test host at 10.0.2.2 (a time server the test runs on the host). The NIC
	// has no gateway, so the Node still has no route off the host. Without it the NIC is
	// restrict=on, as image.md describes.
	HostReach bool
	// Routes is the Node's spec.network.routes.
	Routes []Route
	// NoMasquerade is mvmd's network.no_masquerade: the prefixes whose traffic from a Machine keeps
	// its source address.
	NoMasquerade []string
	// NetEcho is the arguments of a naru-netecho service, which the Node runs when it is not empty.
	NetEcho []string
	// RegistryCA is the PEM authority of the registry service, which the Node's spec.registries
	// names for RegistryHost and mvmd's registry section trusts through ca_file.
	RegistryCA string
	// AgentPair, when not empty, is a host directory the data disk carries as gaon-node-agent/,
	// so that a Node boots with a kept agent pair that init may set aside (ADR 0026).
	AgentPair string
}

// Route is a spec.network.routes entry: a prefix and the address that reaches it.
type Route struct{ To, Via string }

// Network is what a Node's network-config and its configuration say of addresses. The QEMU tests
// use QEMUNetwork; the release's example user-data gives the Proxmox values.
type Network struct {
	// IP is the Node's address, Peer the address its stand-in dials, and MAC its card's.
	IP, Peer, MAC string
	// Netmask, Gateway, Nameservers and Search are what the network-config names.
	Netmask, Gateway string
	Nameservers      []string
	Search           string
	// MachinePrefix is the IPv4 prefix mvmd gives its Machines; the first host address of it is
	// the Machines' gateway and resolver.
	MachinePrefix netip.Prefix
}

// QEMUNetwork is the network of Node index of total on the QEMU tests' shared network: the Nodes
// are 10.78.0.<index>, each dials the next, and each has the Machine prefix 10.77.<index>.0/24.
// The MAC is in the form Proxmox gives its guests' cards: locally administered, and no two Nodes
// of a cluster share one.
func QEMUNetwork(index, total int) Network {
	peer := index
	if total > 0 {
		peer = index%total + 1
	}
	return Network{
		IP:            fmt.Sprintf("10.78.0.%d", index),
		Peer:          fmt.Sprintf("10.78.0.%d", peer),
		MAC:           fmt.Sprintf("02:00:00:00:%02x:%02x", index>>8, index&0xff),
		Netmask:       "255.255.255.0",
		Gateway:       Gateway,
		Nameservers:   []string{Gateway},
		Search:        "naru.test",
		MachinePrefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, 77, byte(index), 0}), 24),
	}
}

// net is the Node's Network: the one the Config carries, or QEMUNetwork's when it carries none.
func (c Config) net() Network {
	if c.Network.IP != "" {
		return c.Network
	}
	return QEMUNetwork(c.Index, c.Total)
}

// IP is the Node's address on the shared network, which its network-config gives it.
func (c Config) IP() string { return c.net().IP }

// RegistryFilesDir is where init writes the registry files (ADR 0026).
const RegistryFilesDir = "/run/naru"

// Gateway is the default gateway and the resolver the QEMU Nodes' network-config names. No machine
// has this address; the Nodes only check that init installs the route and writes the resolver.
const Gateway = "10.78.0.254"

// PeerIP is the address of the Node this Node's stand-in dials: the next one,
// and the first after the last.
func (c Config) PeerIP() string { return c.net().Peer }

// MAC is the Node's network card address; its network-config names it.
func (c Config) MAC() string { return c.net().MAC }

// NetworkConfig is the Node's network-config, in the version 1 form qemu-server's nocloud_network
// writes for a static address, a gateway, nameservers and a search domain.
func (c Config) NetworkConfig() []byte {
	n := c.net()
	var ns strings.Builder
	for _, a := range n.Nameservers {
		fmt.Fprintf(&ns, "      - '%s'\n", a)
	}
	return []byte(fmt.Sprintf(`version: 1
config:
    - type: physical
      name: eth0
      mac_address: '%s'
      subnets:
        - type: static
          address: '%s'
          netmask: '%s'
          gateway: '%s'
    - type: nameserver
      address:
%s      search:
      - '%s'
`, n.MAC, n.IP, n.Netmask, n.Gateway, ns.String(), n.Search))
}

type service struct {
	Name         string            `yaml:"name"`
	Binary       string            `yaml:"binary"`
	Args         []string          `yaml:"args,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	ControlPlane bool              `yaml:"controlPlane,omitempty"`
}

type route struct {
	To  string `yaml:"to"`
	Via string `yaml:"via"`
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
			Routes     []route  `yaml:"routes,omitempty"`
		} `yaml:"network"`
		Services   []service  `yaml:"services"`
		Files      []file     `yaml:"files"`
		Registries []registry `yaml:"registries,omitempty"`
	} `yaml:"spec"`
}

type registry struct {
	Host string `yaml:"host"`
	CA   string `yaml:"ca"`
}

type file struct {
	Path    string `yaml:"path"`
	Mode    string `yaml:"mode,omitempty"`
	Content string `yaml:"content"`
}

// NaruYAML is the Node's naru.yaml, which is its NoCloud user-data, for the Nodes that run the
// tests' own services; a Node that runs gaon has the user-data naructl writes. It carries mvmd's
// configuration as a file for init to write under /run. Its services, in this order: mvmd; the
// registry that serves the test image; a stand-in that waits for mvmd's
// socket and for the next Node's stand-in; a control-plane stand-in, which
// starts only where spec.controlPlane is true; and, where asked, the probe.
func (c Config) NaruYAML() ([]byte, error) {
	mvmdJSON, err := c.MvmdJSON()
	if err != nil {
		return nil, err
	}
	var n nodeConfig
	n.Spec.Files = []file{{Path: mvmdConfig, Mode: "0600", Content: string(mvmdJSON) + "\n"}}
	n.APIVersion = "naru.garam.sh/v1alpha1"
	n.Kind = "NodeConfig"
	n.Metadata.Name = "naru-" + c.Name
	n.Spec.ControlPlane = c.ControlPlane
	n.Spec.DataDisk.Label = DataLabel
	n.Spec.DataDisk.MountPoint = DataMount
	n.Spec.Network.Forwarding = []string{"ipv4"}
	for _, r := range c.Routes {
		n.Spec.Network.Routes = append(n.Spec.Network.Routes, route(r))
	}
	root := fixture.GuestRoot
	if c.RegistryCA != "" {
		n.Spec.Registries = []registry{{Host: fixture.RegistryAddr, CA: c.RegistryCA}}
	}
	mvmd := service{Name: "mvmd", Binary: "/bin/mvmd", Args: []string{"-config", mvmdConfig}}
	registry := service{Name: "registry", Binary: path.Join(root, "bin/naru-registry"), Args: []string{"-listen", fixture.RegistryAddr, "-dir", path.Join(root, "registry")}}
	n.Spec.Services = []service{
		mvmd,
		registry,
		{
			Name: "standin", Binary: "/bin/naru-standin",
			Args: []string{"-socket", MvmdSocket, "-mount", DataMount, "-listen", c.IP() + ":" + StandinPort, "-peer", c.PeerIP() + ":" + StandinPort},
		},
		{Name: "control", Binary: "/bin/naru-standin", ControlPlane: true},
	}
	if len(c.NetEcho) > 0 {
		n.Spec.Services = append(n.Spec.Services, service{Name: "netecho", Binary: path.Join(root, "bin/naru-netecho"), Args: c.NetEcho})
	}
	if c.Probe {
		n.Spec.Services = append(n.Spec.Services, service{
			Name: "probe", Binary: path.Join(root, "bin/naru-probe"),
			Args: []string{
				"-socket", MvmdSocket, "-timeout", "150s", "-supervisor-cgroup", supervisorCgroup,
				"-image-repository", c.Inputs.Image.Repository, "-image-digest", c.Inputs.Image.Digest,
			},
		})
	}
	return yaml.Marshal(n)
}

// MvmdJSON is mvmd's configuration for the Node: the Firecracker driver on
// the guest files of the image, the registry's authority named by its registry
// section, supervisors outside mvmd's cgroup, and a Machine
// network of its own per Node.
func (c Config) MvmdJSON() ([]byte, error) {
	g := c.Inputs.Guest
	// The kernels and the initrds are the image's own; their paths and digests are guest.json's.
	// The Driver takes every declared kernel and the default's name; the converter has one
	// kernel of its own, which is the default.
	kernel := g.Default()
	kernels := make(map[string]any, len(g.Kernels))
	for name, k := range g.Kernels {
		kernels[name] = map[string]any{"path": k.Path, "sha256": k.SHA256}
	}
	prefix := c.net().MachinePrefix
	first := prefix.Masked().Addr().Next().String()
	network := map[string]any{
		"ipv4_prefix":  prefix.String(),
		"ipv4_gateway": first,
		"resolvers":    []string{first},
	}
	if len(c.NoMasquerade) > 0 {
		network["no_masquerade"] = c.NoMasquerade
	}
	registry := map[string]any{"credentials_file": RegistryFilesDir + "/registry/credentials.json"}
	if c.RegistryCA != "" {
		registry["ca_file"] = RegistryFilesDir + "/registry/ca.pem"
	}
	return json.MarshalIndent(map[string]any{
		"registry":  registry,
		"socket":    map[string]any{"path": MvmdSocket, "mode": "0660"},
		"state_dir": DataMount + "/mvmd",
		"driver":    "firecracker",
		"disk":      map[string]any{"mkfs": MkfsPath},
		"firecracker": map[string]any{
			"binary": "/bin/firecracker", "binary_sha256": c.Inputs.FirecrackerSHA256,
			"kernels": kernels, "default_kernel": g.DefaultKernel,
			"initrd": g.GuestInitrd.Path, "initrd_sha256": g.GuestInitrd.SHA256,
		},
		"conversion": map[string]any{
			"kernel": kernel.Path, "kernel_sha256": kernel.SHA256,
			"initrd": g.ConverterInitrd.Path, "initrd_sha256": g.ConverterInitrd.SHA256,
			"converter": "naru-converter",
			"vcpus":     2, "memory_bytes": 512 << 20,
			"output_bytes": 256 << 20, "name_set_bytes": 1 << 20,
			"limit": "5m",
		},
		// supervision.dir is left out, so it is "machines" under state_dir on the data disk:
		// the state directory and the run directories stay on one file system (mvmd ADR 0015).
		"supervision":   map[string]any{"binary": "/bin/mvmd-supervisor", "cgroup": supervisorCgroup},
		"vmm_uid_range": map[string]any{"first": 100000, "count": 1000},
		"network":       network,
		"pull_timeout":  "4m",
		"boot_timeout":  "2m",
		"call_timeout":  "5m",
	}, "", "  ")
}
