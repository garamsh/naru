// Package naructl runs the naructl binary, which owns the layout of a gaon Node (ADR 0015): the
// tests and naru-cluster have it write the cluster's CA, each Node's user-data and the test host's
// client pair. It runs a binary and imports nothing of naructl, so that this module builds
// with GOWORK=off and no replace (ADR 0014).
package naructl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Tool is a naructl binary.
type Tool struct {
	// Bin is the path of the executable.
	Bin string
}

// Build builds naructl from the checkout at root into out, static, as a release does.
func Build(root, out string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", out, "./cmd/naructl") //nolint:gosec // fixed arguments
	cmd.Dir = filepath.Join(root, "naructl")
	cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build naructl: %w: %s", err, b)
	}
	return nil
}

func (t Tool) run(args ...string) error {
	if t.Bin == "" {
		return fmt.Errorf("no naructl binary is named")
	}
	if b, err := exec.Command(t.Bin, args...).CombinedOutput(); err != nil { //nolint:gosec // the binary the caller named
		return fmt.Errorf("naructl %s: %w: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(string(b)))
	}
	return nil
}

// GenCA has naructl write the cluster's CA, ca.pem and ca.key, into dir.
func (t Tool) GenCA(dir string) error { return t.run("gen", "ca", "--out", dir) }

// Client is the files of a client pair and the CA that signed it.
type Client struct {
	CA, Cert, Key string
}

// GenClient has naructl issue a client pair with the CN cn from the CA in caDir, into out.
func (t Tool) GenClient(caDir, cn, out string) (Client, error) {
	if err := t.run("gen", "client", "--ca", caDir, "--cn", cn, "--out", out); err != nil {
		return Client{}, err
	}
	return Client{CA: filepath.Join(caDir, "ca.pem"), Cert: filepath.Join(out, "client.pem"), Key: filepath.Join(out, "client.key")}, nil
}

// Node is the arguments of gen node; empty ones are left to naructl's default or refusal.
type Node struct {
	CA, Name                      string
	ControlPlane, PairsAsFiles    bool
	Address, MAC, Gateway, Search string
	Nameservers                   []string
	ExtraNIC                      string
	APIServer                     string
	ServingAddresses              []string
	// Binaries maps a service's name to its executable's path on the Node.
	Binaries                          map[string]string
	Disable                           []string
	MachinePrefix, Guest, Firecracker string
	Out, NetworkConfig                string
}

// GenNode has naructl write the Node's user-data to n.Out, and its network-config to
// n.NetworkConfig when that is set.
func (t Tool) GenNode(n Node) error {
	args := []string{"gen", "node", "--ca", n.CA, "--name", n.Name, "--address", n.Address,
		"--api-server", n.APIServer, "--machine-prefix", n.MachinePrefix, "--guest", n.Guest,
		"--firecracker-sha256", n.Firecracker, "--out", n.Out}
	flag := func(name string, values ...string) {
		for _, v := range values {
			args = append(args, "--"+name, v)
		}
	}
	if n.ControlPlane {
		args = append(args, "--control-plane")
	}
	if n.PairsAsFiles {
		args = append(args, "--pairs-as-files")
	}
	if n.NetworkConfig != "" {
		flag("network-config", n.NetworkConfig)
		flag("mac", n.MAC)
		flag("gateway", n.Gateway)
		flag("nameserver", n.Nameservers...)
	}
	if n.Search != "" {
		flag("search", n.Search)
	}
	if n.ExtraNIC != "" {
		flag("extra-nic", n.ExtraNIC)
	}
	flag("serving-address", n.ServingAddresses...)
	names := make([]string, 0, len(n.Binaries))
	for name := range n.Binaries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		flag("binary", name+"="+n.Binaries[name])
	}
	flag("disable", n.Disable...)
	return t.run(args...)
}
