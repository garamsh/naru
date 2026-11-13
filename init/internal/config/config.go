// Package config parses and validates a Node's naru.yaml, the file init reads
// at boot (docs/architecture/config.md).
package config

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strconv"
)

const (
	// APIVersion is the only apiVersion init accepts.
	APIVersion = "naru.garam.sh/v1alpha1"
	// Kind is the only kind init accepts.
	Kind = "NodeConfig"
	// FileName names the document in errors: it is the Node's user-data.
	FileName = "naru.yaml"
	// FilesRoot is the directory the files of spec.files are written under.
	FilesRoot = "/run"
	// ReservedDir is init's own directory under FilesRoot, which spec.files may not write to.
	ReservedDir = "/run/naru"
)

// Family is an IP family for which forwarding is turned on.
type Family string

const (
	IPv4 Family = "ipv4"
	IPv6 Family = "ipv6"
)

// NodeConfig is a parsed and validated naru.yaml.
type NodeConfig struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`
}

// Metadata names the Node.
type Metadata struct {
	Name string `yaml:"name"`
}

// Spec is what the Node is configured to run.
type Spec struct {
	Services     []Service `yaml:"services"`
	ControlPlane bool      `yaml:"controlPlane"`
	DataDisk     DataDisk  `yaml:"dataDisk"`
	Network      Network   `yaml:"network"`
	Files        []File    `yaml:"files"`
	PKI          PKI       `yaml:"pki"`
	// Time is how the Node keeps its clock. It is nil when init keeps none.
	Time *Time `yaml:"time,omitempty"`
	// API is the Node API's listener. It is nil when the Node serves none.
	API *API `yaml:"api,omitempty"`
	// Registries are the container registries the Node trusts and presents a credential to.
	Registries []Registry `yaml:"registries,omitempty"`
	// AWS is the identity the credential helpers use. It is nil when the Node has none.
	AWS *AWS `yaml:"aws,omitempty"`
	// Storage lists the extra disks init prepares and mounts for mvmd's disks
	// (docs/architecture/adr/0027-a-node-offers-an-extra-disk-as-xfs-storage-for-mvmds-disks-formatted-when-blank-mounted-hardened-and-grown-at-boot.md).
	Storage []Storage `yaml:"storage,omitempty"`
	// Host is the region and labels naru-hostd reports. It is nil when the Node serves no host API.
	Host *Host `yaml:"host,omitempty"`
}

// StorageRoot is the directory a storage disk is mounted under, as StorageRoot/<name>, unless it names a mount point.
const StorageRoot = "/var/lib/naru/storage"

// StorageFormXFS is the one file system form a storage disk takes.
const StorageFormXFS = "xfs"

// Storage is one extra disk, found by Label or by Device, exactly one of them.
type Storage struct {
	// Name keys the entry and is the file system's label when init formats the disk.
	Name string `yaml:"name"`
	// Label is a GPT partition label or a file system label.
	Label string `yaml:"label,omitempty"`
	// Device is a device path such as /dev/sdb.
	Device string `yaml:"device,omitempty"`
	// Form is the file system, "xfs", which is also what an empty Form means.
	Form string `yaml:"form,omitempty"`
	// MountPoint is where the disk is mounted. It is StorageRoot/<Name> when empty.
	MountPoint string `yaml:"mountPoint,omitempty"`
}

// Mount returns the directory the disk is mounted at.
func (s Storage) Mount() string {
	if s.MountPoint != "" {
		return s.MountPoint
	}
	return StorageRoot + "/" + s.Name
}

// DefaultAPIListen is where the Node API listens when spec.api names no address.
const DefaultAPIListen = "0.0.0.0:50000"

// API is how the Node serves its API (docs/architecture/adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md).
type API struct {
	// Listen is the TCP address, an IP address and a port. It is DefaultAPIListen when empty.
	Listen string `yaml:"listen,omitempty"`
	// ClientCA is the PEM of the certificate authority whose client certificates the API accepts.
	ClientCA string `yaml:"clientCA"`
	// Certificate is the PEM of the serving certificate.
	Certificate string `yaml:"certificate"`
	// Key is the PEM of the serving certificate's private key.
	Key string `yaml:"key"`
}

// ListenAddr returns the address the API listens on.
func (a API) ListenAddr() string {
	if a.Listen == "" {
		return DefaultAPIListen
	}
	return a.Listen
}

// File is a file init writes under FilesRoot before it starts any service.
type File struct {
	Path string `yaml:"path"`
	// Mode is the permission bits in octal, such as "0600". It is "0644" when empty.
	Mode    string `yaml:"mode"`
	Content string `yaml:"content"`
	// Restart names the services init restarts when the file's path, mode or content changes in an apply.
	Restart []string `yaml:"restart,omitempty"`
}

// Perm returns the permission bits of the file. It is only meaningful once
// Parse has accepted the file.
func (f File) Perm() os.FileMode {
	if f.Mode == "" {
		return 0o644
	}
	m, _ := strconv.ParseUint(f.Mode, 8, 32)
	return os.FileMode(m)
}

// Service is an opaque binary init starts and supervises.
type Service struct {
	Name   string            `yaml:"name"`
	Binary string            `yaml:"binary"`
	Args   []string          `yaml:"args"`
	Env    map[string]string `yaml:"env"`
	// ControlPlane marks a service started only on the node that runs the control plane.
	ControlPlane bool `yaml:"controlPlane"`
	// KeptPair names a certificate pair the service keeps, which init checks before it starts the
	// service. It is nil for a service that keeps none.
	KeptPair *KeptPair `yaml:"keptPair,omitempty"`
	// RestartOnChange lists files, each a clean absolute path, that something other than init
	// writes and the service reads once at start. init restarts the service when the content of
	// one changes, or the file appears or goes missing (docs/architecture/adr/0029-aws-credentials-is-a-service-serving-the-instance-metadata-endpoint-to-mvmd.md).
	RestartOnChange []string `yaml:"restartOnChange,omitempty"`
	// LogFiles lists files, each a clean absolute path, that the service or a program it runs
	// writes its log to. init keeps the lines appended to one while the service runs as lines of
	// the service, so `naructl logs` shows them.
	LogFiles []string `yaml:"logFiles,omitempty"`
}

// KeptPair is a certificate pair file a service keeps across boots. A pair whose certificate's
// subject has no Organization of the given value is set aside before the service starts, so the
// service asks for a new one.
type KeptPair struct {
	// File is the pair file: the certificate, then its key, in PEM.
	File string `yaml:"file"`
	// Organization is the Organization the certificate's subject must carry.
	Organization string `yaml:"organization"`
}

// Equal reports whether o is the same service. A nil list or map equals an empty one.
func (s Service) Equal(o Service) bool {
	return s.Name == o.Name && s.Binary == o.Binary && s.ControlPlane == o.ControlPlane &&
		slices.Equal(s.Args, o.Args) && maps.Equal(s.Env, o.Env) &&
		slices.Equal(s.RestartOnChange, o.RestartOnChange) && slices.Equal(s.LogFiles, o.LogFiles) &&
		(s.KeptPair == nil) == (o.KeptPair == nil) && (s.KeptPair == nil || *s.KeptPair == *o.KeptPair)
}

// Runs reports whether init starts svc on this Node: a service marked controlPlane runs only on
// the Node that runs the control plane.
func (s Spec) Runs(svc Service) bool {
	return !svc.ControlPlane || s.ControlPlane
}

// DataDisk names the disk mounted at MountPoint, by Device or by Label, exactly one of them.
type DataDisk struct {
	Device     string `yaml:"device"`
	Label      string `yaml:"label"`
	MountPoint string `yaml:"mountPoint"`
}

// Network holds the families for which forwarding is turned on and the routes init adds.
type Network struct {
	Forwarding []Family `yaml:"forwarding"`
	// Routes are static IPv4 routes, one per destination prefix (docs/architecture/adr/0023-spec-network-routes-are-static-ipv4-routes-init-applies-live.md).
	Routes []Route `yaml:"routes,omitempty"`
}

// Route sends the traffic for To to the next hop Via, which must be on a network the Node is on.
type Route struct {
	To  netip.Prefix `yaml:"to"`
	Via netip.Addr   `yaml:"via"`
}

// FieldError is a validation failure that names the field it concerns, as a
// path from the document root such as spec.dataDisk.mountPoint.
type FieldError struct {
	Field   string
	Message string
}

// Error returns the field and what is wrong with it.
func (e *FieldError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// Fields returns the field of every *FieldError joined in err, in order.
func Fields(err error) []string {
	var out []string
	var walk func(error)
	walk = func(e error) {
		var fe *FieldError
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, c := range j.Unwrap() {
				walk(c)
			}
		} else if errors.As(e, &fe) {
			out = append(out, fe.Field)
		}
	}
	walk(err)
	return out
}
