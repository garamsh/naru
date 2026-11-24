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

	"github.com/garamsh/naru/lib/wire"
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

// RetiredPKI is what a configuration written before spec.pki was removed carries there: two lists,
// which must be empty.
type RetiredPKI struct {
	Authorities  []any `yaml:"authorities,omitempty"`
	Certificates []any `yaml:"certificates,omitempty"`
}

// Spec is what the Node is configured to run.
type Spec struct {
	Services []Service `yaml:"services"`
	// ControlPlane is retired: naru runs no control plane (docs/architecture/adr/0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md).
	// A configuration written before then carries false, which is read as no field; true is refused.
	ControlPlane bool     `yaml:"controlPlane,omitempty"`
	DataDisk     DataDisk `yaml:"dataDisk"`
	Network      Network  `yaml:"network"`
	Files        []File   `yaml:"files"`
	// PKI is retired: init issues no certificate (docs/architecture/adr/0115-init-issues-no-certificates-and-spec-pki-is-removed.md).
	// A configuration written before then carries two empty lists, which are read as no field; an entry is refused.
	PKI RetiredPKI `yaml:"pki,omitempty"`
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
	// Orch makes init run naru-orchd (docs/architecture/adr/0050-the-image-carries-orch-and-the-signer-and-init-runs-them-as-separate-users-from-spec-orch-spec-signer-and-spec-metrics.md). It is nil when the Node runs no orch member.
	Orch *Orch `yaml:"orch,omitempty"`
	// Signer makes init run naru-signer. It is nil when the Node runs no signer.
	Signer *SignerSection `yaml:"signer,omitempty"`
	// Proxy makes init run naru-proxy (docs/architecture/proxy.md). It is nil when the Node runs no proxy.
	Proxy *Proxy `yaml:"proxy,omitempty"`
	// Extensions are the system extension images init mounts read-only before any service starts
	// (docs/architecture/adr/0055-orch-and-the-signer-are-delivered-as-a-system-extension-a-squashfs-image-init-mounts-read-only.md).
	Extensions []Extension `yaml:"extensions,omitempty"`
	// Metrics makes init serve /metrics and gives each daemon its --metrics-addr. It is nil when none serves metrics.
	Metrics *Metrics `yaml:"metrics,omitempty"`
	// Join makes the configuration the join form: the only field of spec, from which init derives
	// what a host that has not joined an orch runs. It is nil in every other configuration.
	Join *Join `yaml:"join,omitempty"`
}

// StorageRoot is the directory a storage disk is mounted under, as StorageRoot/<name>, unless it names a mount point.
const StorageRoot = wire.DataMount + "/storage"

// StorageFormXFS is the one file system form a storage disk takes.
const StorageFormXFS = "xfs"

// Storage is one extra disk, found by Label, by Device or by Serial, exactly one of them.
type Storage struct {
	// Name keys the entry and is the file system's label when init formats the disk.
	Name string `yaml:"name"`
	// Label is a GPT partition label or a file system label.
	Label string `yaml:"label,omitempty"`
	// Device is a device path such as /dev/sdb.
	Device string `yaml:"device,omitempty"`
	// Serial is the serial number of a blank disk (a virtio-blk or SCSI disk's), which init
	// formats at the first boot; later boots find it by its XFS label, the Name.
	Serial string `yaml:"serial,omitempty"`
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
const DefaultAPIListen = "0.0.0.0:" + wire.NodeAPIPort

// API is how the Node serves its API (docs/architecture/adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md).
type API struct {
	// Listen is the TCP address, an IP address and a port. It is DefaultAPIListen when empty.
	Listen string `yaml:"listen,omitempty"`
	// ClientCA is the PEM of the certificate authority whose client certificates the API accepts.
	ClientCA string `yaml:"clientCA"`
	// Certificate is the PEM of the serving certificate.
	Certificate string `yaml:"certificate"`
	// Key is the PEM of the serving certificate's private key. Exactly one of Key and KeyFile is set.
	Key string `yaml:"key,omitempty"`
	// KeyFile is the absolute path of a PEM private key that the Node API reads when it starts, in
	// place of Key. The file is not read when the configuration is parsed.
	KeyFile string `yaml:"keyFile,omitempty"`
	// RevocationPublicKey is the PKIX PEM of the key that signs the revocation list. With it the API
	// refuses a client certificate on the list, and every client while no list newer than
	// RevocationMaxAge can be had (docs/architecture/adr/0062-every-naru-tls-peer-is-checked-against-the-signed-revocation-list-by-one-verifier-in-lib-identity-and-fails-closed-when-the-list-is-older-than-its-max-age.md).
	// Without it no revocation is checked. The list is read from the signer on this Node when
	// spec.signer is set, and otherwise fetched from orch, at spec.host.orch.
	RevocationPublicKey string `yaml:"revocationPublicKey,omitempty"`
	// RevocationMaxAge is a Go duration: how long a list that cannot be refreshed is used. It is
	// DefaultRevocationMaxAge when empty.
	RevocationMaxAge string `yaml:"revocationMaxAge,omitempty"`
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
	// Owner is the user and group that own the file. It is root's when nil.
	Owner *Owner `yaml:"owner,omitempty"`
	// Template names what Content is rendered from when init writes the file: "guest" renders it as a
	// Go text/template over the booted image's guest.json (see Render). It is empty for a file written as
	// it stands.
	Template string `yaml:"template,omitempty"`
}

// Owner is a numeric user and group, a file's owner.
type Owner struct {
	UID uint32 `yaml:"uid"`
	GID uint32 `yaml:"gid"`
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
	Name string `yaml:"name"`
	// ControlPlane is retired: naru runs no control plane (docs/architecture/adr/0114-naru-runs-no-kubernetes-or-gaon-and-their-layouts-programs-images-tests-and-documents-are-removed.md).
	// A configuration written before then carries false, which is read as no field; true is refused.
	ControlPlane bool              `yaml:"controlPlane,omitempty"`
	Binary       string            `yaml:"binary"`
	Args         []string          `yaml:"args"`
	Env          map[string]string `yaml:"env"`
	// RestartOnChange lists files, each a clean absolute path, that something other than init
	// writes and the service reads once at start. init restarts the service when the content of
	// one changes, or the file appears or goes missing (docs/architecture/adr/0029-aws-credentials-is-a-service-serving-the-instance-metadata-endpoint-to-mvmd.md).
	RestartOnChange []string `yaml:"restartOnChange,omitempty"`
	// LogFiles lists files, each a clean absolute path, that the service or a program it runs
	// writes its log to. init keeps the lines appended to one while the service runs as lines of
	// the service, so `naructl logs` shows them.
	LogFiles []string `yaml:"logFiles,omitempty"`
	// User is the user the service runs as. It runs as root when nil.
	User *User `yaml:"user,omitempty"`
	// Dirs are directories init makes, owns and gives a mode before each start of the service.
	Dirs []Dir `yaml:"dirs,omitempty"`
	// Setup are commands that init runs one after another to completion, as User, before a start
	// of the service, each unless a path it names exists. A setup that fails keeps the service
	// from starting.
	Setup []Setup `yaml:"setup,omitempty"`
	// RestartPolicy is RestartAlways, which is also what it is when empty, or RestartOnFailure.
	RestartPolicy string `yaml:"restartPolicy,omitempty"`
	// BindLowPorts lets a service that runs as User listen on a port below 1024: init gives it
	// CAP_NET_BIND_SERVICE as an ambient capability, and no other.
	BindLowPorts bool `yaml:"bindLowPorts,omitempty"`
}

// User is the identity a service runs with: a user, a group and the supplementary groups.
type User struct {
	UID    uint32   `yaml:"uid"`
	GID    uint32   `yaml:"gid"`
	Groups []uint32 `yaml:"groups,omitempty"`
}

// Dir is a directory a service needs, which init makes with its parents when it is missing and
// whose owner and mode it sets on every start of the service.
type Dir struct {
	Path string `yaml:"path"`
	// Mode is the permission bits in octal. It is "0755" when empty.
	Mode string `yaml:"mode,omitempty"`
	UID  uint32 `yaml:"uid"`
	GID  uint32 `yaml:"gid"`
}

// Perm returns the permission bits of the directory. It is only meaningful once Parse has accepted it.
func (d Dir) Perm() os.FileMode {
	if d.Mode == "" {
		return 0o755
	}
	m, _ := strconv.ParseUint(d.Mode, 8, 32)
	return os.FileMode(m)
}

// Setup is a one-shot command of a service, run with the service's binary.
type Setup struct {
	// Args are the arguments of the service's binary.
	Args []string `yaml:"args"`
	// Unless are clean absolute paths: the setup does not run while any of them exists.
	Unless []string `yaml:"unless,omitempty"`
}

// Equal reports whether o is the same service. A nil list or map equals an empty one.
func (s Service) Equal(o Service) bool {
	return s.Name == o.Name && s.Binary == o.Binary && s.RestartPolicy == o.RestartPolicy && s.BindLowPorts == o.BindLowPorts &&
		slices.Equal(s.Args, o.Args) && maps.Equal(s.Env, o.Env) &&
		slices.Equal(s.RestartOnChange, o.RestartOnChange) && slices.Equal(s.LogFiles, o.LogFiles) &&
		(s.User == nil) == (o.User == nil) && (s.User == nil || s.User.equal(*o.User)) &&
		slices.Equal(s.Dirs, o.Dirs) &&
		slices.EqualFunc(s.Setup, o.Setup, func(a, b Setup) bool { return slices.Equal(a.Unless, b.Unless) && slices.Equal(a.Args, b.Args) })
}

func (u User) equal(o User) bool {
	return u.UID == o.UID && u.GID == o.GID && slices.Equal(u.Groups, o.Groups)
}

// DataDisk names the disk mounted at MountPoint, by Device or by Label, exactly one of them.
// FormatBlank asks init to format a blank disk as the data disk, which a guest Node's host
// cannot do for it; it needs Label, the label the new file system gets.
type DataDisk struct {
	Device      string `yaml:"device"`
	Label       string `yaml:"label"`
	MountPoint  string `yaml:"mountPoint"`
	FormatBlank bool   `yaml:"formatBlank,omitempty"`
	// FirstBlank is set by init for the join form, and no file carries it: with FormatBlank, the
	// first blank disk that is not the boot disk is formatted, not only the one blank disk.
	FirstBlank bool `yaml:"-"`
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
