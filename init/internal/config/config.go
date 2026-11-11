// Package config parses and validates a Node's naru.yaml, the file init reads
// at boot (docs/architecture/config.md).
package config

import "fmt"

const (
	// APIVersion is the only apiVersion init accepts.
	APIVersion = "naru.garam.sh/v1alpha1"
	// Kind is the only kind init accepts.
	Kind = "NodeConfig"
	// FileName is the file on the configuration disk.
	FileName = "naru.yaml"
	// Label is the file system label of the configuration disk.
	Label = "NARUCFG"
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
