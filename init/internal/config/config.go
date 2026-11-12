// Package config parses and validates a Node's naru.yaml, the file init reads
// at boot (docs/architecture/config.md).
package config

import (
	"errors"
	"fmt"
	"maps"
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
}

// Equal reports whether o is the same service. A nil list or map equals an empty one.
func (s Service) Equal(o Service) bool {
	return s.Name == o.Name && s.Binary == o.Binary && s.ControlPlane == o.ControlPlane &&
		slices.Equal(s.Args, o.Args) && maps.Equal(s.Env, o.Env)
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

// Network holds the families for which forwarding is turned on.
type Network struct {
	Forwarding []Family `yaml:"forwarding"`
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
