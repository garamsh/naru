// Package volumes is the CSI driver naru-volumes (docs/architecture/adr/0083): it makes a
// PersistentVolume of a directory on a Node's storage disk, with a project quota for its size, or of
// a whole disk the Node's configuration lists, and publishes it into a Pod by a bind mount.
package volumes

import (
	"encoding/json"
	"fmt"
	"os"
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

// Config says what the Node offers. The Node's layout writes it.
type Config struct {
	// StateDir keeps one record per volume, outside the storage disk's quota.
	StateDir string `json:"stateDir"`
	// StorageDir is where the first storage disk is mounted, or empty when the Node has none.
	StorageDir string `json:"storageDir,omitempty"`
	// BlockSerials are the serial numbers of the disks the Node offers whole, in the order they are
	// handed out.
	BlockSerials []string `json:"blockSerials,omitempty"`
}

// LoadConfig reads the configuration file at path.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.StateDir == "" {
		return Config{}, fmt.Errorf("%s: stateDir is empty", path)
	}
	return c, nil
}

// Quota limits the blocks a directory's tree may use.
type Quota interface {
	// Enabled returns an error unless the file system holding dir enforces project quotas.
	Enabled(dir string) error
	// Set gives dir the project and limits the project to bytes; dir's files and the ones made in
	// it take the project. A limit of zero lifts it.
	Set(dir string, project uint32, bytes int64) error
}

// Mounter binds and unbinds the directories and device nodes a Pod sees.
type Mounter interface {
	Bind(source, target string, readOnly bool) error
	Unmount(target string) error
	Mounted(target string) (bool, error)
}

// Disk is a whole disk of the Node.
type Disk struct {
	// Path is its device node and Bytes its size.
	Path  string
	Bytes int64
}

// Disks finds a disk by its serial number; an absent disk is an error.
type Disks interface {
	Find(serial string) (Disk, error)
}

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
