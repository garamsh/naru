// Package clusterfile is cluster.json, the file naru-cluster writes once every Node is up and a
// harness reads to reach the cluster. docs/architecture/harness.md is the contract: Version changes
// only when a field is removed, renamed or changes meaning; a field added is optional and leaves
// Version as it is.
package clusterfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Name is the file's name inside naru-cluster's -dir.
const Name = "cluster.json"

// Version is the version of the schema this package writes.
const Version = 1

// File is the content of cluster.json. Every path is absolute.
type File struct {
	// Version is the schema version.
	Version int `json:"version"`
	// APIServer says where the gaon API server is reached from the host.
	APIServer APIServer `json:"apiServer"`
	// PKI names the files a client needs to dial it.
	PKI PKI `json:"pki"`
	// Nodes lists the Nodes in order, node-1 first.
	Nodes []Node `json:"nodes"`
	// Images lists the images the Nodes' mvmd can pull.
	Images []Image `json:"images"`
}

// APIServer is the API server's address on the host.
type APIServer struct {
	// Address is "127.0.0.1:port", the host side of the forward to node-1's API server.
	Address string `json:"address"`
}

// PKI names the CA that signed the API server's certificate and a client pair the CA signed.
type PKI struct {
	CA         string `json:"ca"`
	ClientCert string `json:"clientCert"`
	ClientKey  string `json:"clientKey"`
}

// Node is one QEMU Node.
type Node struct {
	Name string `json:"name"`
	// PID is the QEMU process's; a kill of it is a power-off of the Node.
	PID int `json:"pid"`
	// SerialLog is the file the Node's serial console is written to.
	SerialLog string `json:"serialLog"`
	// ControlPlane is whether the Node's spec.controlPlane is true.
	ControlPlane bool `json:"controlPlane"`
}

// Image is an image the Nodes' registry serves.
type Image struct {
	// Reference is "repository@sha256:...", which an image pull by digest names.
	Reference string `json:"reference"`
}

// Write writes f as dir/cluster.json, all at once: a reader sees the whole file or none.
func Write(dir string, f File) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+Name+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil { //nolint:gosec // no key material in it
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, Name))
}

// Read reads the cluster.json at path and refuses a version it does not know.
func Read(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	if f.Version != Version {
		return File{}, fmt.Errorf("%s has version %d; this reader knows %d", path, f.Version, Version)
	}
	return f, nil
}
