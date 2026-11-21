// Package volumes is the CSI driver naru-volumes (docs/architecture/adr/0083): it makes a
// PersistentVolume of a directory on a Node's storage disk, with a project quota for its size, or of
// a whole disk the Node's configuration lists, and publishes it into a Pod by a bind mount.
package volumes

import (
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	// Name is the driver's name, which the StorageClasses name as their provisioner.
	Name = "naru-volumes"
	// TopologyKey is the label a volume's node affinity names: the Node that holds it.
	TopologyKey = Name + "/node"

	kindLocal = "local"
	kindBlock = "block"
)

// record is what the driver keeps of a volume it made.
type record struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	// Project is a local volume's quota project; Serial a block volume's disk.
	Project uint32 `json:"project,omitempty"`
	Serial  string `json:"serial,omitempty"`
}

// Driver implements the CSI identity, controller and node services of one Node.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	node  string
	cfg   Config
	quota Quota
	mnt   Mounter
	disks Disks
	// mu orders the changes of the records.
	mu sync.Mutex
}

// New returns the driver of the Node named node.
func New(node string, cfg Config, q Quota, m Mounter, d Disks) *Driver {
	return &Driver{node: node, cfg: cfg, quota: q, mnt: m, disks: d}
}
