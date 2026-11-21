package node

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// The Kubernetes cluster of ADR 0031: two naru Nodes, node-1 the control-plane Node and node-2 a
// worker, which run containerd, the kubelet and kube-proxy as init services. naructl writes every
// user-data; the Pod prefixes come from one Pod network.
const (
	// KubePodNetwork is the prefix every Pod address is in: a /16 of 10.64.0.0/12, since the
	// harness's Nodes are on 10.78.0.0/24, which the /12 contains.
	KubePodNetwork = "10.64.0.0/16"
	// KubeNodeMemoryMiB is the memory of a Kubernetes Node: the initramfs holds Kubernetes.
	KubeNodeMemoryMiB = 4096
)

// KubeSpec says what StartKubernetes boots.
type KubeSpec struct {
	// Disk, Dir, Inputs and Naructl are the ClusterSpec's.
	Disk, Dir string
	Inputs    fixture.Inputs
	Naructl   naructl.Tool
	// Disable names services left out of every Node's configuration.
	Disable []string
	// DisableAddons names the add-ons the cluster leaves out (gen kubernetes --disable-addon).
	DisableAddons []string
	// Resolver makes node-1's address the resolver of every Node, so that a Pod on node-1 listening on
	// port 53 answers the names the Nodes and the cluster's DNS forward outside the cluster.
	Resolver bool
	// Late numbers the workers (3 and up) StartLate starts, not StartKubernetes. A cluster with them
	// has the control-plane Node accept bootstrap tokens and serve a Node API (APIAdmin), through
	// which `naructl token create --kubernetes` adds a Late worker's token live (ADR 0081); the
	// authority's directory stays until Stop.
	Late []int
	// VolumeDisk, when "storage" or "block", gives every Node a blank extra disk of KubeVolumeDiskBytes
	// with the serial KubeVolumeSerial, which the Node's configuration makes the disk of the local
	// volumes (naru-local: XFS, mounted by init, the first storage disk) or offers whole as a block
	// volume (naru-block), so that the volume add-on has a disk to serve (ADR 0083).
	VolumeDisk string
}

// The extra disk of a cluster that serves volumes (KubeSpec.VolumeDisk).
const (
	KubeVolumeDiskBytes = 1 << 30
	KubeVolumeSerial    = "NARUVOL0001"
	VolumeDiskStorage   = "storage"
	VolumeDiskBlock     = "block"
)

// KubeCluster is the two Nodes and the administrator's kubeconfig, which dials the control-plane
// Node's API server through the forward on the test host.
type KubeCluster struct {
	Nodes []*Node
	// Kubeconfig is the path of admin.conf, which carries a key; it lives under the spec's Dir.
	Kubeconfig string
	// Image is the reference of the test image, which a Pod runs and the sandbox is.
	Image string
	// CADir is the authority's directory, whose tokens/ the token commands change, and APIAdmin the
	// client file of node-1's Node API; both are set with KubeSpec.Late.
	CADir, APIAdmin string

	spec                             KubeSpec
	caDir, apiServer, sandbox, mcast string
	total, hostPort                  int
}

// KubePodPrefix is the Pod prefix of Node index: 10.64.<index>.0/24, in KubePodNetwork.
func KubePodPrefix(index int) string { return fmt.Sprintf("10.64.%d.0/24", index) }

// StartKubernetes boots node-1 as the control-plane Node and node-2 as a worker, and returns them as
// soon as QEMU has started each. The caller must Stop it.
func StartKubernetes(spec KubeSpec) (*KubeCluster, error) {
	if spec.Inputs.Image.Digest == "" {
		return nil, errors.New("a Kubernetes cluster needs the test image, which its Pods run")
	}
	for _, i := range spec.Late {
		if i < 3 {
			return nil, errors.New("Late numbers workers after node-2")
		}
	}
	total := 2 + len(spec.Late)
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("pick a host port for the API server: %w", err)
	}
	caDir := filepath.Join(spec.Dir, "ca")
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return nil, err
	}
	// The CA's directory goes once the Nodes' user-data is written: it holds the authority's key, the
	// ServiceAccount key and admin.conf. The administrator keeps a copy of admin.conf.
	if len(spec.Late) == 0 {
		defer func() { _ = os.RemoveAll(caDir) }()
	}
	if err := spec.Naructl.GenCA(caDir); err != nil {
		return nil, err
	}
	apiServer := QEMUNetwork(1, total).IP
	if err := spec.Naructl.GenKubernetes(caDir, apiServer, "127.0.0.1:"+strconv.Itoa(port), spec.DisableAddons...); err != nil {
		return nil, err
	}
	var apiPort int
	var apiAdmin string
	if len(spec.Late) > 0 {
		// The Node API's authority makes every Node's user-data carry spec.api; node-1's is reached
		// through a forward.
		if apiPort, err = freePort(); err != nil {
			return nil, fmt.Errorf("pick a host port for the Node API: %w", err)
		}
		if err := spec.Naructl.GenAPICA(caDir); err != nil {
			return nil, err
		}
		dir := filepath.Join(spec.Dir, "api-client")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		apiAdmin = filepath.Join(dir, "admin.yaml")
		if err := spec.Naructl.GenAPIClient(caDir, "admin", "127.0.0.1:"+strconv.Itoa(apiPort), apiAdmin); err != nil {
			return nil, err
		}
	}
	admin, err := os.ReadFile(filepath.Join(caDir, "admin.conf"))
	if err != nil {
		return nil, err
	}
	c := &KubeCluster{
		Kubeconfig: filepath.Join(spec.Dir, "admin.conf"),
		Image:      spec.Inputs.Image.Repository + "@" + spec.Inputs.Image.Digest,
		spec:       spec, caDir: caDir, apiServer: apiServer, total: total, hostPort: port, APIAdmin: apiAdmin,
	}
	if len(spec.Late) > 0 {
		c.CADir = caDir
	}
	// The sandbox runs the image's command with no arguments, which would serve the microVM probe on
	// the port the test's server listens on.
	sandbox := c.Image
	if spec.Inputs.Sandbox.Digest != "" {
		sandbox = spec.Inputs.Sandbox.Repository + "@" + spec.Inputs.Sandbox.Digest
	}
	if err := os.WriteFile(c.Kubeconfig, admin, 0o600); err != nil {
		return nil, err
	}
	c.sandbox, c.mcast = sandbox, fmt.Sprintf("230.78.0.1:%d", 20000+rand.IntN(40000))
	for i := 1; i <= total; i++ {
		if slices.Contains(spec.Late, i) {
			continue
		}
		if err := c.startNode(i, apiPort); err != nil {
			c.Stop()
			return nil, err
		}
	}
	return c, nil
}

// startNode writes the user-data of Node i and starts it. apiPort is the host port forwarded to node-1's
// Node API, when the cluster has one.
func (c *KubeCluster) startNode(i, apiPort int) error {
	cfg := Config{Name: fmt.Sprintf("node-%d", i), Index: i, Total: c.total, ControlPlane: i == 1, Inputs: c.spec.Inputs, MemoryMiB: KubeNodeMemoryMiB}
	if c.spec.VolumeDisk != "" {
		cfg.ExtraDisk, cfg.VirtioDisks, cfg.ExtraDiskSerial = KubeVolumeDiskBytes, true, KubeVolumeSerial
	}
	if i == 1 {
		cfg.HostPort = c.hostPort // only the control-plane Node has the second NIC
		cfg.APIHostPort = apiPort
	}
	start := Spec{Config: cfg, Disk: c.spec.Disk, Dir: filepath.Join(c.spec.Dir, cfg.Name), Mcast: c.mcast}
	var err error
	if start.NaruYAML, start.NetworkConfig, err = kubeNode(cfg, c.caDir, c.apiServer, c.sandbox, c.spec); err != nil {
		return fmt.Errorf("write the user-data of %s: %w", cfg.Name, err)
	}
	n, err := Start(start)
	if err != nil {
		return fmt.Errorf("start %s: %w", cfg.Name, err)
	}
	c.Nodes = append(c.Nodes, n)
	return nil
}

// StartLate starts Late worker i, which presents the token <CADir>/tokens/node-<i> holds, which the
// test made with `naructl token create --kubernetes` before.
func (c *KubeCluster) StartLate(i int) (*Node, error) {
	if c.CADir == "" || !slices.Contains(c.spec.Late, i) {
		return nil, fmt.Errorf("node-%d is not a Late worker of this cluster", i)
	}
	if err := c.startNode(i, 0); err != nil {
		return nil, err
	}
	return c.Nodes[len(c.Nodes)-1], nil
}

// kubeNode has naructl write the user-data and network-config of the Node c names.
func kubeNode(c Config, caDir, apiServer, sandbox string, spec KubeSpec) (userData, networkConfig []byte, err error) {
	n := c.net()
	ones, _ := net.IPMask(net.ParseIP(n.Netmask).To4()).Size()
	dir := filepath.Join(spec.Dir, c.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	role := "worker"
	if c.ControlPlane {
		role = "control-plane"
	}
	if spec.Resolver {
		n.Nameservers = []string{QEMUNetwork(1, c.Total).IP}
	}
	k := naructl.KubernetesNode{
		CA: caDir, Name: c.Name, Role: role, Address: n.IP + "/" + strconv.Itoa(ones), Endpoint: apiServer,
		PodNetwork: KubePodNetwork, PodPrefix: KubePodPrefix(c.Index),
		MAC: n.MAC, Gateway: n.Gateway, Nameservers: n.Nameservers, Search: n.Search,
		LocalRegistry: true, RegistryCA: path.Join(fixture.GuestRoot, "registry", "ca.pem"), SandboxImage: sandbox,
		Disable: spec.Disable,
		Out:     filepath.Join(dir, "naru.yaml"), NetworkConfig: filepath.Join(dir, "network-config"),
	}
	for j := 1; j <= c.Total; j++ {
		if j != c.Index {
			k.Peers = append(k.Peers, QEMUNetwork(j, c.Total).IP+"="+KubePodPrefix(j))
		}
	}
	late := len(spec.Late) > 0
	k.BootstrapTokens = late && c.ControlPlane
	k.Token = slices.Contains(spec.Late, c.Index)
	if c.hostNIC() {
		k.ExtraNIC = c.HostMAC() + "=" + HostNICPrefix
	}
	switch spec.VolumeDisk {
	case "":
	case VolumeDiskStorage:
		k.Storage = []string{"data1=serial:" + KubeVolumeSerial}
	case VolumeDiskBlock:
		k.BlockSerials = []string{KubeVolumeSerial}
	default:
		return nil, nil, fmt.Errorf("VolumeDisk is %q, not %q or %q", spec.VolumeDisk, VolumeDiskStorage, VolumeDiskBlock)
	}
	if err := spec.Naructl.GenKubernetesNode(k); err != nil {
		return nil, nil, err
	}
	if userData, err = os.ReadFile(k.Out); err != nil {
		return nil, nil, err
	}
	networkConfig, err = os.ReadFile(k.NetworkConfig)
	return userData, networkConfig, err
}

// Stop ends every Node, and removes the authority's directory a cluster with Late workers kept.
func (c *KubeCluster) Stop() {
	if c.CADir != "" {
		defer func() { _ = os.RemoveAll(c.CADir) }()
	}
	for _, n := range c.Nodes {
		n.Stop()
	}
}
