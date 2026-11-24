// Package naructl runs the naructl binary, which owns the layout of a Node (ADR 0015): the tests
// have it write the authorities, each Node's user-data and the test host's client files. It runs a
// binary and imports nothing of naructl, so that the binary of a release is driven as the
// checkout's is.
package naructl

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Tool is a naructl binary.
type Tool struct {
	// Bin is the path of the executable.
	Bin string
	// Legacy is set for the naructl of a release made before naructl moved to cobra (v0.1.0-dev.11
	// and earlier; see ForRelease), which knows only the flag spellings it was released with: gen node
	// takes --ca for its authorities' directory, and gen ca --out for the directory it writes. A later
	// naructl keeps those as hidden aliases for one release only.
	Legacy bool
	// Gaon is set for the naructl of a release made before naru ran no gaon (v0.1.0-dev.14 and
	// earlier; see ForRelease), whose gen node writes a Node of a gaon cluster from the cluster's
	// authority and the API server's address. GenNode has it make that authority and write a worker
	// whose pairs are files and whose gaon services are left out: the user-data this checkout's
	// init boots (ADR 0114).
	Gaon bool
}

// lastLegacyDev is the number of the last v0.1.0-dev.N release whose naructl knows only the flag
// spellings from before cobra; dev.12 keeps them as hidden aliases until dev.13. lastGaonDev is the
// number of the last one whose naructl writes gaon Nodes.
const (
	lastLegacyDev = 11
	lastGaonDev   = 14
)

// gaonServices are the services of a gaon Node that the naructl of a Gaon Tool leaves out.
var gaonServices = []string{"etcd", "gaon-apiserver", "gaon-scheduler", "gaon-controllers", "gaon-node-agent"}

// ForRelease is the Tool for the naructl at bin that the release of that version shipped: a release
// of v0.1.0-dev.11 or earlier is driven with the spellings it was released with, and one of
// v0.1.0-dev.14 or earlier as one that writes gaon Nodes. Any other version, including one that is
// not a v0.1.0-dev.N, is driven as the checkout's naructl.
func ForRelease(bin, version string) Tool {
	n, ok := strings.CutPrefix(version, "v0.1.0-dev.")
	dev, err := strconv.Atoi(n)
	dated := ok && err == nil
	return Tool{Bin: bin, Legacy: dated && dev <= lastLegacyDev, Gaon: dated && dev <= lastGaonDev}
}

// caDirFlag is the flag that names the directory of the CA the gen command reads.
func (t Tool) caDirFlag() string {
	if t.Legacy {
		return "--ca"
	}
	return "--ca-dir"
}

// Build builds naructl from the checkout at root into out, static, as a release does.
func Build(root, out string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", out, "./cmd/naructl")
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
	cmd := exec.Command(t.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("naructl %s: %w: %s", strings.Join(args[:min(len(args), 2)], " "), err, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return stdout.Bytes(), nil
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
	cmd := exec.Command(t.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return Result{Stdout: stdout.String(), Stderr: stderr.String(), Err: err}
}

// Node is the arguments of gen node; empty ones are left to naructl's default or refusal.
type Node struct {
	// CA is the authorities' directory: gen api-ca's and gen host-ca's.
	CA, Name                      string
	Address, MAC, Gateway, Search string
	Nameservers                   []string
	ExtraNIC                      string
	ServingAddresses              []string
	// TimeServers are host[:port] entries of spec.time.servers (gen node --time-server).
	TimeServers []string
	// Peers are <address>=<machine-prefix> entries (gen node --peer).
	Peers []string
	// Extensions are gen node --extension values: <name> or <name>=<SHA-256>.
	Extensions []string
	// Registries are gen node --registry values: host=H,ca=FILE,username=U,password=P or helper=NAME.
	Registries []string
	// AWSRegion, AWSTrustAnchor, AWSProfile and AWSRole are gen node's --aws-* flags, all or none.
	AWSRegion, AWSTrustAnchor, AWSProfile, AWSRole string
	// HostRegion and HostLabels (key=value) are gen node's --host-region and --host-label.
	HostRegion string
	HostLabels []string
	// Storage are <name>=<device or label> entries (gen node --storage), the first holding mvmd's
	// disks, snapshots and run directories.
	Storage []string
	// Extra are further gen node flags, each as given on the command line ("--signer").
	Extra                      []string
	MachinePrefix, Firecracker string
	Out, NetworkConfig         string
}

// GenNode has naructl write the Node's user-data to n.Out, and its network-config to
// n.NetworkConfig when that is set.
func (t Tool) GenNode(n Node) error {
	args := []string{"gen", "node", t.caDirFlag(), n.CA, "--name", n.Name, "--address", n.Address,
		"--machine-prefix", n.MachinePrefix, "--firecracker-sha256", n.Firecracker, "--out", n.Out}
	flag := func(name string, values ...string) {
		for _, v := range values {
			args = append(args, "--"+name, v)
		}
	}
	if t.Gaon {
		if err := t.genClusterCA(n.CA); err != nil {
			return err
		}
		apiServer, _, _ := strings.Cut(n.Address, "/")
		flag("api-server", apiServer)
		args = append(args, "--pairs-as-files")
		flag("disable", gaonServices...)
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
	flag("time-server", n.TimeServers...)
	flag("peer", n.Peers...)
	flag("extension", n.Extensions...)
	flag("registry", n.Registries...)
	flag("storage", n.Storage...)
	if n.AWSRegion != "" {
		flag("aws-region", n.AWSRegion)
		flag("aws-trust-anchor", n.AWSTrustAnchor)
		flag("aws-profile", n.AWSProfile)
		flag("aws-role", n.AWSRole)
	}
	if n.HostRegion != "" {
		flag("host-region", n.HostRegion)
		flag("host-label", n.HostLabels...)
	}
	args = append(args, n.Extra...)
	return t.run(args...)
}

// genClusterCA has the naructl of a Gaon Tool write the cluster's authority, ca.pem and ca.key, into
// dir, which its gen node needs, unless dir holds one.
func (t Tool) genClusterCA(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "ca.pem")); err == nil {
		return nil
	}
	flag := "--ca-dir"
	if t.Legacy {
		flag = "--out"
	}
	return t.run("gen", "ca", flag, dir)
}
