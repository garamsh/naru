package node

import (
	"encoding/json"
	"fmt"
	"path"

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
}

// IP is the Node's address on the shared network, which its network-config gives it.
func (c Config) IP() string { return fmt.Sprintf("10.78.0.%d", c.Index) }

// Gateway is the default gateway and the resolver its network-config names. No machine has this
// address; the Nodes only check that init installs the route and writes the resolver.
const Gateway = "10.78.0.254"

// PeerIP is the address of the Node this Node's stand-in dials: the next one,
// and the first after the last.
func (c Config) PeerIP() string { return fmt.Sprintf("10.78.0.%d", c.Index%c.Total+1) }

// MAC is the Node's network card address, in the form Proxmox gives its guests' cards: locally
// administered, and no two Nodes of a cluster share one. The Node's network-config names it.
func (c Config) MAC() string { return fmt.Sprintf("02:00:00:00:%02x:%02x", c.Index>>8, c.Index&0xff) }

// NetworkConfig is the Node's network-config, in the version 1 form qemu-server's nocloud_network
// writes for a static address, a gateway, a nameserver and a search domain.
func (c Config) NetworkConfig() []byte {
	return []byte(fmt.Sprintf(`version: 1
config:
    - type: physical
      name: eth0
      mac_address: '%s'
      subnets:
        - type: static
          address: '%s'
          netmask: '255.255.255.0'
          gateway: '%s'
    - type: nameserver
      address:
      - '%s'
      search:
      - 'naru.test'
`, c.MAC(), c.IP(), Gateway, Gateway))
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
	n.Metadata.Name = "naru5-" + c.Name
	n.Spec.ControlPlane = c.ControlPlane
	n.Spec.DataDisk.Label = DataLabel
	n.Spec.DataDisk.MountPoint = DataMount
	n.Spec.Network.Forwarding = []string{"ipv4"}
	root := fixture.GuestRoot
	n.Spec.Services = []service{
		{
			Name: "mvmd", Binary: "/bin/mvmd", Args: []string{"-config", mvmdConfig},
			Env: map[string]string{"SSL_CERT_FILE": path.Join(root, "registry/ca.pem")},
		},
		{Name: "registry", Binary: path.Join(root, "bin/naru-registry"), Args: []string{"-listen", fixture.RegistryAddr, "-dir", path.Join(root, "registry")}},
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
// the staged guest files, the registry's authority reached through the
// service's SSL_CERT_FILE, supervisors outside mvmd's cgroup, and a Machine
// network of its own per Node.
func (c Config) MvmdJSON() ([]byte, error) {
	root := fixture.GuestRoot
	kernel := path.Join(root, "guest/vmlinux")
	return json.MarshalIndent(map[string]any{
		"socket":    map[string]any{"path": MvmdSocket, "mode": "0660"},
		"state_dir": DataMount + "/state",
		"driver":    "firecracker",
		"disk":      map[string]any{"mkfs": path.Join(root, "bin/mkfs.ext4")},
		"firecracker": map[string]any{
			"binary": "/bin/firecracker", "binary_sha256": c.Inputs.FirecrackerSHA256,
			"kernel": kernel, "kernel_sha256": c.Inputs.KernelSHA256,
			"initrd": path.Join(root, "guest/guest-initrd.cpio"), "initrd_sha256": c.Inputs.GuestInitrdSHA256,
		},
		"conversion": map[string]any{
			"kernel": kernel, "kernel_sha256": c.Inputs.KernelSHA256,
			"initrd": path.Join(root, "guest/converter-initrd.cpio"), "initrd_sha256": c.Inputs.ConverterInitrdSHA256,
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
			"ipv4_prefix":  fmt.Sprintf("10.77.%d.0/24", c.Index),
			"ipv4_gateway": fmt.Sprintf("10.77.%d.1", c.Index),
			"resolvers":    []string{fmt.Sprintf("10.77.%d.1", c.Index)},
		},
		"pull_timeout": "4m",
		"boot_timeout": "2m",
		"call_timeout": "5m",
	}, "", "  ")
}
