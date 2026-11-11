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
	// Gaon, when set, makes the Node run gaon instead of the test services (see NaruYAML).
	Gaon *Gaon
}

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
%s    - type: nameserver
      address:
%s      search:
      - '%s'
`, n.MAC, n.IP, n.Netmask, n.Gateway, c.hostNICConfig(), ns.String(), n.Search))
}

// hostNICConfig is the network-config entry of the second NIC, or nothing for a Node with none: a
// static address on the user-mode network and no gateway, so that the default route stays on the
// shared network.
func (c Config) hostNICConfig() string {
	if !c.hostNIC() {
		return ""
	}
	return fmt.Sprintf(`    - type: physical
      name: eth1
      mac_address: '%s'
      subnets:
        - type: static
          address: '%s'
          netmask: '%s'
`, c.hostMAC(), HostNICAddress, hostNICNetmask)
}

type service struct {
	Name         string            `yaml:"name"`
	Binary       string            `yaml:"binary"`
	Args         []string          `yaml:"args,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	ControlPlane bool              `yaml:"controlPlane,omitempty"`
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
	} `yaml:"spec"`
}

type file struct {
	Path    string `yaml:"path"`
	Mode    string `yaml:"mode,omitempty"`
	Content string `yaml:"content"`
}

// NaruYAML is the Node's naru.yaml, which is its NoCloud user-data. It carries mvmd's
// configuration as a file for init to write under /run. Its services, in this order: mvmd; the
// registry that serves the test image; a stand-in that waits for mvmd's
// socket and for the next Node's stand-in; a control-plane stand-in, which
// starts only where spec.controlPlane is true; and, where asked, the probe. A Node with Gaon set
// runs mvmd, the registry (the scheduler may bind a Machine to any Node, and mvmd pulls from the
// registry on its own Node) and then gaon's services (gaon.go) in place of the rest, and its files
// carry the certificates.
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
	root := fixture.GuestRoot
	mvmd := service{
		Name: "mvmd", Binary: "/bin/mvmd", Args: []string{"-config", mvmdConfig},
		Env: map[string]string{"SSL_CERT_FILE": path.Join(root, "registry/ca.pem")},
	}
	registry := service{Name: "registry", Binary: path.Join(root, "bin/naru-registry"), Args: []string{"-listen", fixture.RegistryAddr, "-dir", path.Join(root, "registry")}}
	if c.Gaon != nil {
		n.Spec.Files = append(n.Spec.Files, c.Gaon.files(c)...)
		n.Spec.Services = append([]service{mvmd, registry}, c.Gaon.services(c)...)
		return yaml.Marshal(n)
	}
	n.Spec.Services = []service{
		mvmd,
		registry,
		{
			Name: "standin", Binary: "/bin/naru-standin",
			Args: []string{"-socket", MvmdSocket, "-mount", DataMount, "-listen", c.IP() + ":" + StandinPort, "-peer", c.PeerIP() + ":" + StandinPort},
		},
		{Name: "control", Binary: "/bin/naru-standin", ControlPlane: true},
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
// the guest files of the image, the registry's authority reached through the
// service's SSL_CERT_FILE, supervisors outside mvmd's cgroup, and a Machine
// network of its own per Node.
func (c Config) MvmdJSON() ([]byte, error) {
	g := c.Inputs.Guest
	// The kernel and the initrds are the image's own; their paths and digests are guest.json's.
	kernel := g.Default()
	prefix := c.net().MachinePrefix
	first := prefix.Masked().Addr().Next().String()
	return json.MarshalIndent(map[string]any{
		"socket":    map[string]any{"path": MvmdSocket, "mode": "0660"},
		"state_dir": DataMount + "/mvmd",
		"driver":    "firecracker",
		"disk":      map[string]any{"mkfs": MkfsPath},
		"firecracker": map[string]any{
			"binary": "/bin/firecracker", "binary_sha256": c.Inputs.FirecrackerSHA256,
			"kernel": kernel.Path, "kernel_sha256": kernel.SHA256,
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
		"network": map[string]any{
			"ipv4_prefix":  prefix.String(),
			"ipv4_gateway": first,
			"resolvers":    []string{first},
		},
		"pull_timeout": "4m",
		"boot_timeout": "2m",
		"call_timeout": "5m",
	}, "", "  ")
}
