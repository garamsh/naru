package node

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"path"
	"path/filepath"
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
}

// KubeCluster is the two Nodes and the administrator's kubeconfig, which dials the control-plane
// Node's API server through the forward on the test host.
type KubeCluster struct {
	Nodes []*Node
	// Kubeconfig is the path of admin.conf, which carries a key; it lives under the spec's Dir.
	Kubeconfig string
	// Image is the reference of the test image, which a Pod runs and the sandbox is.
	Image string
}

// KubePodPrefix is the Pod prefix of Node index: 10.64.<index>.0/24, in KubePodNetwork.
func KubePodPrefix(index int) string { return fmt.Sprintf("10.64.%d.0/24", index) }

// StartKubernetes boots node-1 as the control-plane Node and node-2 as a worker, and returns them as
// soon as QEMU has started each. The caller must Stop it.
func StartKubernetes(spec KubeSpec) (*KubeCluster, error) {
	if spec.Inputs.Image.Digest == "" {
		return nil, errors.New("a Kubernetes cluster needs the test image, which its Pods run")
	}
	const total = 2
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("pick a host port for the API server: %w", err)
	}
	caDir := filepath.Join(spec.Dir, "ca")
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil { //nolint:gosec // a test directory
		return nil, err
	}
	// The CA's directory goes once the Nodes' user-data is written: it holds the authority's key, the
	// ServiceAccount key and admin.conf. The administrator keeps a copy of admin.conf.
	defer func() { _ = os.RemoveAll(caDir) }()
	if err := spec.Naructl.GenCA(caDir); err != nil {
		return nil, err
	}
	apiServer := QEMUNetwork(1, total).IP
	if err := spec.Naructl.GenKubernetes(caDir, apiServer, "127.0.0.1:"+strconv.Itoa(port)); err != nil {
		return nil, err
	}
	admin, err := os.ReadFile(filepath.Join(caDir, "admin.conf"))
	if err != nil {
		return nil, err
	}
	c := &KubeCluster{
		Kubeconfig: filepath.Join(spec.Dir, "admin.conf"),
		Image:      spec.Inputs.Image.Repository + "@" + spec.Inputs.Image.Digest,
	}
	if err := os.WriteFile(c.Kubeconfig, admin, 0o600); err != nil {
		return nil, err
	}
	mcast := fmt.Sprintf("230.78.0.1:%d", 20000+rand.IntN(40000)) //nolint:gosec // a port, not a secret
	for i := 1; i <= total; i++ {
		cfg := Config{Name: fmt.Sprintf("node-%d", i), Index: i, Total: total, ControlPlane: i == 1, Inputs: spec.Inputs, MemoryMiB: KubeNodeMemoryMiB}
		if i == 1 {
			cfg.HostPort = port // only the control-plane Node has the second NIC
		}
		start := Spec{Config: cfg, Disk: spec.Disk, Dir: filepath.Join(spec.Dir, cfg.Name), Mcast: mcast}
		if start.NaruYAML, start.NetworkConfig, err = kubeNode(cfg, caDir, apiServer, c.Image, spec); err != nil {
			c.Stop()
			return nil, fmt.Errorf("write the user-data of %s: %w", cfg.Name, err)
		}
		n, err := Start(start)
		if err != nil {
			c.Stop()
			return nil, fmt.Errorf("start %s: %w", cfg.Name, err)
		}
		c.Nodes = append(c.Nodes, n)
	}
	return c, nil
}

// kubeNode has naructl write the user-data and network-config of the Node c names.
func kubeNode(c Config, caDir, apiServer, image string, spec KubeSpec) (userData, networkConfig []byte, err error) {
	n := c.net()
	ones, _ := net.IPMask(net.ParseIP(n.Netmask).To4()).Size()
	dir := filepath.Join(spec.Dir, c.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test directory
		return nil, nil, err
	}
	other := 3 - c.Index // the other Node of the two
	role := "worker"
	if c.ControlPlane {
		role = "control-plane"
	}
	k := naructl.KubernetesNode{
		CA: caDir, Name: c.Name, Role: role, Address: n.IP + "/" + strconv.Itoa(ones), Endpoint: apiServer,
		PodNetwork: KubePodNetwork, PodPrefix: KubePodPrefix(c.Index),
		Peers: []string{QEMUNetwork(other, 2).IP + "=" + KubePodPrefix(other)},
		MAC:   n.MAC, Gateway: n.Gateway, Nameservers: n.Nameservers, Search: n.Search,
		LocalRegistry: true, RegistryCA: path.Join(fixture.GuestRoot, "registry", "ca.pem"), SandboxImage: image,
		Disable: spec.Disable,
		Out:     filepath.Join(dir, "naru.yaml"), NetworkConfig: filepath.Join(dir, "network-config"),
	}
	if c.hostNIC() {
		k.ExtraNIC = c.HostMAC() + "=" + HostNICPrefix
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

// Stop ends every Node.
func (c *KubeCluster) Stop() {
	for _, n := range c.Nodes {
		n.Stop()
	}
}
