package node

import (
	"encoding/json"
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

// OverrideDir is where the data disk carries the gaon binaries a harness supplies, as a Node sees
// it: the paths naructl is told through gen node --binary.
const OverrideDir = fixture.GuestRoot + "/gaon"

// ClusterSpec says what N Nodes on one network boot.
type ClusterSpec struct {
	// N is the number of Nodes; their names are node-1 to node-N.
	N int
	// Disk is the image's naru.img, which the Nodes boot under OVMF.
	Disk string
	// Dir receives one directory per Node.
	Dir string
	// Inputs is the staged tree every Node's data disk carries.
	Inputs fixture.Inputs
	// ProbeOn is the number of the Node that runs the probe, or 0 for none.
	ProbeOn int
	// Gaon runs gaon on the Nodes, with the user-data and the certificates Naructl writes (ADR 0015):
	// the Node Agent on each, and etcd, the API server, the scheduler and the controllers on Node 1.
	Gaon bool
	// Naructl is the naructl binary, which Gaon requires.
	Naructl naructl.Tool
	// BootPKI, with Gaon, has Node 1 issue its certificates from the CA its spec.pki carries and sign
	// the agents' requests with it, and every Node's agent get its pair through a bootstrap token
	// (ADR 0013, ADR 0016). Without it every pair of every Node is an operator's file.
	BootPKI bool
	// Disable names services left out of every Node's configuration; naructl refuses a name it
	// does not know.
	Disable []string
	// GaonBinaries names the gaon services whose binary the Inputs tree carries under gaon/.
	GaonBinaries []string
}

// Cluster is the Nodes of one network. Node 1 is the one whose configuration
// sets spec.controlPlane.
type Cluster struct {
	Nodes []*Node
	// Client and HostAddr are set when the cluster runs gaon: the test host's client pair with the
	// CA that signed it, and the address on the test host, "127.0.0.1:port", that reaches the API server.
	Client   naructl.Client
	HostAddr string
}

// StartCluster boots N Nodes that share a network and returns them as soon as
// QEMU has started each. The caller must Stop it.
func StartCluster(spec ClusterSpec) (*Cluster, error) {
	if spec.N < 1 {
		return nil, errors.New("a cluster needs at least one Node")
	}
	// The Nodes of one cluster meet on a multicast group and port of their own, so that
	// clusters running side by side on one host do not share a network.
	mcast := fmt.Sprintf("230.78.0.1:%d", 20000+rand.IntN(40000)) //nolint:gosec // a port, not a secret
	c := &Cluster{}
	var gen *gaonFiles
	if spec.Gaon {
		var err error
		if gen, err = newGaonFiles(spec); err != nil {
			return nil, err
		}
		c.Client, c.HostAddr = gen.client, "127.0.0.1:"+strconv.Itoa(gen.port)
		// The CA's key stays on disk only while naructl needs it, whichever way StartCluster returns.
		defer func() { _ = gen.dropCA() }()
	}
	for i := 1; i <= spec.N; i++ {
		name := fmt.Sprintf("node-%d", i)
		cfg := Config{
			Name: name, Index: i, Total: spec.N, ControlPlane: i == 1, Probe: i == spec.ProbeOn, Inputs: spec.Inputs,
		}
		start := Spec{Config: cfg, Disk: spec.Disk, Dir: filepath.Join(spec.Dir, name), Mcast: mcast}
		if gen != nil {
			if i == 1 {
				start.Config.HostPort = gen.port // only Node 1 has the second NIC
			}
			var err error
			if start.NaruYAML, start.NetworkConfig, err = gen.node(start.Config, spec); err != nil {
				c.Stop()
				return nil, fmt.Errorf("write the user-data of %s: %w", name, err)
			}
		}
		n, err := Start(start)
		if err != nil {
			c.Stop()
			return nil, fmt.Errorf("start %s: %w", name, err)
		}
		c.Nodes = append(c.Nodes, n)
	}
	return c, nil
}

// Stop ends every Node.
func (c *Cluster) Stop() {
	for _, n := range c.Nodes {
		n.Stop()
	}
}

// freePort returns a TCP port on the loopback that nothing listens on now.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// gaonFiles are what naructl wrote for a gaon cluster: the CA, the test host's client pair and the
// guest manifest the Nodes' mvmd configuration is made from.
type gaonFiles struct {
	caDir, guest string
	client       naructl.Client
	port         int
}

func newGaonFiles(spec ClusterSpec) (*gaonFiles, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("pick a host port for the API server: %w", err)
	}
	g := &gaonFiles{caDir: filepath.Join(spec.Dir, "ca"), guest: filepath.Join(spec.Dir, "guest.json"), port: port}
	manifest, err := json.Marshal(spec.Inputs.Guest)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(spec.Dir, 0o755); err != nil { //nolint:gosec // a test directory
		return nil, err
	}
	if err := os.WriteFile(g.guest, manifest, 0o600); err != nil {
		return nil, err
	}
	if err := spec.Naructl.GenCA(g.caDir); err != nil {
		return nil, err
	}
	clientDir := filepath.Join(spec.Dir, "client")
	if g.client, err = spec.Naructl.GenClient(g.caDir, "naru-test-host", clientDir); err != nil {
		return nil, err
	}
	if spec.BootPKI {
		// Node 1's user-data, written first, lists the token of every Node.
		for i := 1; i <= spec.N; i++ {
			if err := spec.Naructl.GenToken(g.caDir, fmt.Sprintf("node-%d", i)); err != nil {
				return nil, err
			}
		}
	}
	// The CA's directory is removed once the last Node's user-data is written, key and all, so the
	// client keeps its own copy of the certificate.
	cert, err := os.ReadFile(g.client.CA)
	if err != nil {
		return nil, err
	}
	g.client.CA = filepath.Join(clientDir, "ca.pem")
	if err := os.WriteFile(g.client.CA, cert, 0o644); err != nil { //nolint:gosec // a certificate
		return nil, err
	}
	return g, nil
}

// dropCA removes the CA's directory: no file under -dir holds the CA's own key once the Nodes'
// user-data is written, except a control-plane Node's user-data that issues its certificates at boot.
func (g *gaonFiles) dropCA() error { return os.RemoveAll(g.caDir) }

// node has naructl write the user-data and network-config of the Node c names, on the shared
// network. The serving certificate names the Nodes' address and the host forward's.
func (g *gaonFiles) node(c Config, spec ClusterSpec) (userData, networkConfig []byte, err error) {
	n := c.net()
	ones, _ := net.IPMask(net.ParseIP(n.Netmask).To4()).Size()
	dir := filepath.Join(spec.Dir, c.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a test directory
		return nil, nil, err
	}
	a := naructl.Node{
		CA: g.caDir, Name: c.Name, ControlPlane: c.ControlPlane, PairsAsFiles: !spec.BootPKI,
		Address: n.IP + "/" + strconv.Itoa(ones), MAC: n.MAC, Gateway: n.Gateway, Nameservers: n.Nameservers, Search: n.Search,
		APIServer: QEMUNetwork(1, spec.N).IP, MachinePrefix: n.MachinePrefix.String(),
		Guest: g.guest, Firecracker: spec.Inputs.FirecrackerSHA256,
		Disable: spec.Disable, Binaries: map[string]string{},
		Out: filepath.Join(dir, "naru.yaml"), NetworkConfig: filepath.Join(dir, "network-config"),
	}
	if c.hostNIC() {
		a.ExtraNIC = c.HostMAC() + "=" + HostNICPrefix
	}
	if c.ControlPlane {
		a.ServingAddresses = []string{"127.0.0.1"}
	}
	for _, name := range spec.GaonBinaries {
		a.Binaries[name] = path.Join(OverrideDir, name)
	}
	if err := spec.Naructl.GenNode(a); err != nil {
		return nil, nil, err
	}
	if userData, err = os.ReadFile(a.Out); err != nil {
		return nil, nil, err
	}
	networkConfig, err = os.ReadFile(a.NetworkConfig)
	return userData, networkConfig, err
}
