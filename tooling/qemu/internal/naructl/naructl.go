// Package naructl runs the naructl binary, which owns the layout of a gaon Node (ADR 0015): the
// tests and naru-cluster have it write the cluster's CA, each Node's user-data and the test host's
// client pair. It runs a binary and imports nothing of naructl, so that this module builds
// with GOWORK=off and no replace (ADR 0014).
package naructl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	_, err := t.output(args...)
	return err
}

// output runs naructl and returns its standard output; a failure carries what it printed.
func (t Tool) output(args ...string) ([]byte, error) {
	if t.Bin == "" {
		return nil, fmt.Errorf("no naructl binary is named")
	}
	cmd := exec.Command(t.Bin, args...) //nolint:gosec // the binary the caller named
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("naructl %s: %w: %s", strings.Join(args[:min(len(args), 2)], " "), err, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return stdout.Bytes(), nil
}

// Service is one of the services a Node runs besides mvmd.
type Service struct {
	Name string `json:"name"`
	// ControlPlane is set for a service only the control-plane Node runs.
	ControlPlane bool `json:"controlPlane"`
	// Gaon is set for a service that is one of gaon's binaries.
	Gaon bool `json:"gaon"`
}

// Topology is what naructl says a Node runs, in the order init starts it, and which service signs the
// agents' requests on a control-plane Node that issues its certificates (ADR 0019).
type Topology struct {
	Services []Service `json:"services"`
	Signer   string    `json:"signer"`
	// APIServer and Agent are the services a harness needs both of to ask the API whether the Nodes
	// have registered.
	APIServer string `json:"apiserver"`
	Agent     string `json:"agent"`
}

// Topology asks naructl for its topology.
func (t Tool) Topology() (Topology, error) {
	b, err := t.output("topology")
	if err != nil {
		return Topology{}, fmt.Errorf("%w (a naructl made before ADR 0019 has no topology command)", err)
	}
	var top Topology
	if err := json.Unmarshal(b, &top); err != nil {
		return Topology{}, fmt.Errorf("naructl topology: %w", err)
	}
	if len(top.Services) == 0 || top.Signer == "" || top.APIServer == "" || top.Agent == "" {
		return Topology{}, fmt.Errorf("naructl topology names no services or no signer, API server or agent: %s", strings.TrimSpace(string(b)))
	}
	return top, nil
}

// Names are the services' names, in order.
func (t Topology) Names() []string {
	names := make([]string, len(t.Services))
	for i, s := range t.Services {
		names[i] = s.Name
	}
	return names
}

// Has reports whether name is one of the services.
func (t Topology) Has(name string) bool { return slices.Contains(t.Names(), name) }

// GaonNames are the names of gaon's own binaries among the services.
func (t Topology) GaonNames() []string {
	var names []string
	for _, s := range t.Services {
		if s.Gaon {
			names = append(names, s.Name)
		}
	}
	return names
}

// Running are the services a Node runs, in order: all of them on the control-plane Node, and the
// others elsewhere.
func (t Topology) Running(controlPlane bool) []Service {
	return slices.DeleteFunc(slices.Clone(t.Services), func(s Service) bool { return s.ControlPlane && !controlPlane })
}

// CheckNames refuses a name that is not one of the services.
func (t Topology) CheckNames(names []string) error {
	for _, n := range names {
		if !t.Has(n) {
			return fmt.Errorf("%q is not a service; the names are %s", n, strings.Join(t.Names(), ", "))
		}
	}
	return nil
}

// GenCA has naructl write the cluster's CA, ca.pem and ca.key, into dir.
func (t Tool) GenCA(dir string) error { return t.run("gen", "ca", "--out", dir) }

// GenToken has naructl write the bootstrap token of the Node of that name next to the CA in caDir,
// if there is none. The control-plane Node's user-data lists the tokens there are when it is written,
// so every Node's token is made before the first gen node.
func (t Tool) GenToken(caDir, name string) error {
	return t.run("gen", "token", "--ca", caDir, "--name", name)
}

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

// GenEtcdClient has naructl write the etcd client pair of gaon's snapshot writer from the CA in
// caDir: ca.crt, tls.crt and tls.key in out.
func (t Tool) GenEtcdClient(caDir, out string) error {
	return t.run("gen", "etcd-client", "--ca", caDir, "--out", out)
}

// GenAPICA has naructl write the Node API's authority, api/ca.pem and api/ca.key, under dir. A Node's
// user-data written from dir afterwards carries spec.api (ADR 0017).
func (t Tool) GenAPICA(dir string) error { return t.run("gen", "api-ca", "--ca-dir", dir) }

// GenAPIClient has naructl write the Node API client file out, with the certificate of role (admin or
// reader) from the authority under caDir, for the Node at endpoint (an IP address with a port).
func (t Tool) GenAPIClient(caDir, role, endpoint, out string) error {
	return t.run("gen", "api-client", "--ca-dir", caDir, "--role", role, "--endpoint", endpoint, "--out", out)
}

// GenHostCA has naructl write the host API's authority, host/ca.pem and host/ca.key, under dir. A
// Node's user-data written with a region from dir afterwards carries spec.host (ADR 0028).
func (t Tool) GenHostCA(dir string) error { return t.run("gen", "host-ca", "--ca-dir", dir) }

// GenHostClient has naructl write the host API client file out, with the certificate of role
// (provisioner or reader) from the authority under caDir, for the host at endpoint (an IP address
// with a port).
func (t Tool) GenHostClient(caDir, role, endpoint, out string) error {
	return t.run("gen", "host-client", "--ca-dir", caDir, "--role", role, "--endpoint", endpoint, "--out", out)
}

// Result is what a run of naructl printed and how it ended.
type Result struct {
	Stdout, Stderr string
	// Err is nil when naructl exited 0.
	Err error
}

// Run runs naructl with args and returns what it printed; a refusal is a Result with Err set, not
// an error of the caller's.
func (t Tool) Run(args ...string) Result {
	if t.Bin == "" {
		return Result{Err: fmt.Errorf("no naructl binary is named")}
	}
	cmd := exec.Command(t.Bin, args...) //nolint:gosec // the binary the caller named
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return Result{Stdout: stdout.String(), Stderr: stderr.String(), Err: err}
}

// Node is the arguments of gen node; empty ones are left to naructl's default or refusal.
type Node struct {
	CA, Name     string
	ControlPlane bool
	// PairsAsFiles carries operator-issued pairs as files in place of a bootstrap token.
	PairsAsFiles                  bool
	Address, MAC, Gateway, Search string
	Nameservers                   []string
	ExtraNIC                      string
	APIServer                     string
	ServingAddresses              []string
	// Members are the <name>=<address> of every control-plane Node of a control plane of several.
	Members []string
	// TimeServers are host[:port] entries of spec.time.servers (gen node --time-server).
	TimeServers []string
	// Peers are <address>=<machine-prefix> entries (gen node --peer).
	Peers []string
	// Registries are gen node --registry values: host=H,ca=FILE,username=U,password=P or helper=NAME.
	Registries []string
	// AWSRegion, AWSTrustAnchor, AWSProfile and AWSRole are gen node's --aws-* flags, all or none.
	AWSRegion, AWSTrustAnchor, AWSProfile, AWSRole string
	// HostRegion and HostLabels (key=value) are gen node's --host-region and --host-label.
	HostRegion string
	HostLabels []string
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
	flag("member", n.Members...)
	flag("time-server", n.TimeServers...)
	flag("peer", n.Peers...)
	flag("registry", n.Registries...)
	if n.AWSRegion != "" {
		flag("aws-region", n.AWSRegion)
		flag("aws-trust-anchor", n.AWSTrustAnchor)
		flag("aws-profile", n.AWSProfile)
		flag("aws-role", n.AWSRole)
	}
	names := make([]string, 0, len(n.Binaries))
	for name := range n.Binaries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		flag("binary", name+"="+n.Binaries[name])
	}
	flag("disable", n.Disable...)
	if n.HostRegion != "" {
		flag("host-region", n.HostRegion)
		flag("host-label", n.HostLabels...)
	}
	return t.run(args...)
}
