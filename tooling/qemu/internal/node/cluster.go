package node

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
)

// ClusterSpec says what N Nodes on one network boot.
type ClusterSpec struct {
	// N is the number of Nodes; their names are node-1 to node-N.
	N int
	// Kernel and Initrd are the image's bzImage and initramfs.cpio.gz.
	Kernel, Initrd string
	// Dir receives one directory per Node.
	Dir string
	// Inputs is the staged tree every Node's data disk carries.
	Inputs fixture.Inputs
	// ProbeOn is the number of the Node that runs the probe, or 0 for none.
	ProbeOn int
}

// Cluster is the Nodes of one network. Node 1 is the one whose configuration
// sets spec.controlPlane.
type Cluster struct {
	Nodes []*Node
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
	for i := 1; i <= spec.N; i++ {
		name := fmt.Sprintf("node-%d", i)
		n, err := Start(Spec{
			Config: Config{
				Name: name, Index: i, Total: spec.N, ControlPlane: i == 1, Probe: i == spec.ProbeOn, Inputs: spec.Inputs,
			},
			Kernel: spec.Kernel, Initrd: spec.Initrd, Dir: filepath.Join(spec.Dir, name), Mcast: mcast,
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
