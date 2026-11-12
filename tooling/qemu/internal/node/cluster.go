package node

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"path/filepath"
	"strconv"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/pki"
)

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
	// Gaon runs gaon on the Nodes: the Node Agent on each, and etcd, the API server, the scheduler and
	// the controllers on Node 1, with the certificates the cluster issues (ADR 0012).
	Gaon bool
	// BootPKI, with Gaon, has Node 1 issue its certificates from the CA its spec.pki carries; the
	// other Nodes carry their agent's pair as files (ADR 0013).
	BootPKI bool
}

// Cluster is the Nodes of one network. Node 1 is the one whose configuration
// sets spec.controlPlane.
type Cluster struct {
	Nodes []*Node
	// Certs and HostAddr are set when the cluster runs gaon: the certificates issued for it, and the
	// address on the test host, "127.0.0.1:port", that reaches the API server.
	Certs    *pki.Cluster
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
	var gaon *Gaon
	if spec.Gaon {
		port, err := freePort()
		if err != nil {
			return nil, fmt.Errorf("pick a host port for the API server: %w", err)
		}
		names := make([]string, spec.N)
		for i := range names {
			names[i] = fmt.Sprintf("node-%d", i+1)
		}
		apiIP := Config{Index: 1}.IP()
		// The serving certificate names the Nodes' address and the host forward's.
		if c.Certs, err = pki.NewCluster(names, net.ParseIP(apiIP), net.ParseIP("127.0.0.1")); err != nil {
			return nil, fmt.Errorf("issue the certificates: %w", err)
		}
		c.HostAddr = "127.0.0.1:" + strconv.Itoa(port)
		gaon = &Gaon{Certs: c.Certs, APIServerIP: apiIP, BootPKI: spec.BootPKI}
		gaon.HostPort = port
	}
	for i := 1; i <= spec.N; i++ {
		name := fmt.Sprintf("node-%d", i)
		cfg := Config{
			Name: name, Index: i, Total: spec.N, ControlPlane: i == 1, Probe: i == spec.ProbeOn, Inputs: spec.Inputs,
		}
		if gaon != nil {
			g := *gaon
			if i != 1 {
				g.HostPort = 0 // only Node 1 has the second NIC
			}
			cfg.Gaon = &g
		}
		n, err := Start(Spec{
			Config: cfg, Disk: spec.Disk, Dir: filepath.Join(spec.Dir, name), Mcast: mcast,
		})
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
