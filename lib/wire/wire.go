// Package wire holds the values two parts of naru agree on without calling each other: where the
// data disk is and what it is called, the EFI system partition's GUID, the ports, the form of
// an enrollment key, the header of orch's fencing token and the protocols of an exposed port. Each is defined here once and imported by every Go user.
package wire

import (
	"encoding/hex"
	"slices"
	"strings"
)

// The data disk and the programs the image carries.
const (
	// DataLabel and DataMount are the data disk's file system label and its mount point.
	DataLabel = "NARUDATA"
	DataMount = "/var/lib/naru"
	// ResetMarker is the file naru-hostd writes to ask init for a reset at its next boot, in init's
	// store directory on the data disk (ADR 0082). Both read this one path.
	ResetMarker = DataMount + "/config/reset.json"
	// MvmdStateDir is mvmd's state directory, on the data disk, whose file system has fs-verity for
	// the Image Store (ADR 0003).
	MvmdStateDir = DataMount + "/mvmd"
	// MvmdStorageMount is where the first storage disk is mounted. It holds what mvmd keeps on one
	// XFS with reflinks: the disks, the snapshots and the Machines' run directories (ADR 0073).
	MvmdStorageMount = DataMount + "/mvmd-storage"
	// MkfsPath is the mkfs.ext4 the image carries (ADR 0011), which init runs and mvmd's disk.mkfs names.
	MkfsPath = "/bin/mkfs.ext4"
	// GuestManifest is guest.json, which the image carries beside the guest kernels and initrds it
	// names (ADR 0010). A file of spec.files with `template: guest` is rendered from it at each write,
	// so the digests a configuration names are the booted image's own.
	GuestManifest = "/usr/lib/naru/guest/guest.json"
	// ESPGUID is the fixed partition GUID of the EFI system partition of the image's disk, which
	// image/host/disk/disk.env also holds as ESP_GUID (ADR 0020). The partition is found by it, not
	// by a device name.
	ESPGUID = "cb4162e7-9d1e-48df-b937-f5792071b950"
)

// The TCP ports of the APIs and of etcd.
const (
	// HostAPIPort is the port naru-hostd serves the host API on (ADR 0028).
	HostAPIPort = "7443"
	// NodeAPIPort is the port init serves the Node API on by default (ADR 0017).
	NodeAPIPort = "50000"
	// KubeAPIPort is the port gaon's API server serves mutual TLS on.
	KubeAPIPort = "6443"
	// EtcdClientPort and EtcdPeerPort are etcd's client and peer ports (ADR 0024).
	EtcdClientPort = "2379"
	EtcdPeerPort   = "2380"
)

// Protocol is the transport of an exposed port, spelled as Kubernetes spells a Service port's. Orch's
// API, the host API and naru-hostd take it in this one spelling and no other (ADR 0107).
type Protocol string

// The protocols an exposure carries.
const (
	ProtocolTCP Protocol = "TCP"
	ProtocolUDP Protocol = "UDP"
)

// Protocols returns every Protocol, in order.
func Protocols() []Protocol { return []Protocol{ProtocolTCP, ProtocolUDP} }

// Valid reports whether p is one of Protocols.
func (p Protocol) Valid() bool { return slices.Contains(Protocols(), p) }

// LeaderTokenHeader names the header in which orch's leader sends its fencing token with every
// mutating call to a host (ADR 0071). The value is the decimal token; a call without it is an
// operator's.
const LeaderTokenHeader = "X-Naru-Leader-Token"

// EnrollKeyPrefix starts every enrollment key orch issues; the rest is the hex of EnrollKeyBytes
// random bytes, in lowercase.
const (
	EnrollKeyPrefix = "naru-ek-"
	EnrollKeyBytes  = 32
)

// EnrollKey returns the enrollment key made of secret.
func EnrollKey(secret [EnrollKeyBytes]byte) string {
	return EnrollKeyPrefix + hex.EncodeToString(secret[:])
}

// IsEnrollKey reports whether k has the form EnrollKey returns.
func IsEnrollKey(k string) bool {
	rest, ok := strings.CutPrefix(k, EnrollKeyPrefix)
	if !ok || len(rest) != 2*EnrollKeyBytes {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil && rest == strings.ToLower(rest)
}
