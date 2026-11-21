package naructl

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// KubernetesTopology asks naructl for the topology of a Kubernetes Node (ADR 0031). It has no signer.
func (t Tool) KubernetesTopology() (Topology, error) {
	b, err := t.output("topology", "kubernetes")
	if err != nil {
		return Topology{}, err
	}
	var top Topology
	if err := json.Unmarshal(b, &top); err != nil {
		return Topology{}, fmt.Errorf("naructl topology kubernetes: %w", err)
	}
	if len(top.Services) == 0 || top.APIServer == "" || top.Agent == "" {
		return Topology{}, fmt.Errorf("naructl topology kubernetes names no services, API server or agent: %s", strings.TrimSpace(string(b)))
	}
	return top, nil
}

// GenKubernetes has naructl write the rest of a Kubernetes cluster's files beside the CA in caDir:
// the front-proxy authority, the ServiceAccount keys and admin.conf, which dials adminAddress
// (host:port) when that is set.
func (t Tool) GenKubernetes(caDir, endpoint, adminAddress string, disableAddons ...string) error {
	return t.GenKubernetesCluster(KubernetesCluster{CADir: caDir, Endpoint: endpoint, AdminAddress: adminAddress, DisableAddons: disableAddons})
}

// KubernetesCluster is the arguments of gen kubernetes; empty ones are left to naructl's default.
type KubernetesCluster struct {
	CADir, Endpoint, AdminAddress string
	DisableAddons                 []string
	// ServiceAccountIssuers are --service-account-issuer, the first of which signs, and
	// ServiceAccountAlgorithm is --service-account-algorithm.
	ServiceAccountIssuers   []string
	ServiceAccountAlgorithm string
	// RotateServiceAccountKey is --rotate-service-account-key: the key id of the signing key it replaces.
	RotateServiceAccountKey string
}

// GenKubernetesCluster has naructl run gen kubernetes with k.
func (t Tool) GenKubernetesCluster(k KubernetesCluster) error {
	args := []string{"gen", "kubernetes", t.caDirFlag(), k.CADir, "--endpoint", k.Endpoint}
	if k.AdminAddress != "" {
		args = append(args, "--admin-address", k.AdminAddress)
	}
	for _, name := range k.DisableAddons {
		args = append(args, "--disable-addon", name)
	}
	for _, issuer := range k.ServiceAccountIssuers {
		args = append(args, "--service-account-issuer", issuer)
	}
	if k.ServiceAccountAlgorithm != "" {
		args = append(args, "--service-account-algorithm", k.ServiceAccountAlgorithm)
	}
	if k.RotateServiceAccountKey != "" {
		args = append(args, "--rotate-service-account-key", k.RotateServiceAccountKey)
	}
	return t.run(args...)
}

// SigningKeyID returns the key id of the ServiceAccount key that signs, which `gen kubernetes-jwks` prints
// for the cluster of caDir whose first issuer is set.
func (t Tool) SigningKeyID(caDir string) (string, error) {
	out, err := os.MkdirTemp("", "jwks-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(out) }()
	r := t.Run("gen", "kubernetes-jwks", t.caDirFlag(), caDir, "--out-dir", out)
	if r.Err != nil {
		return "", fmt.Errorf("gen kubernetes-jwks: %w: %s", r.Err, r.Stderr)
	}
	m := signingKeyID.FindStringSubmatch(r.Stdout)
	if m == nil {
		return "", fmt.Errorf("gen kubernetes-jwks printed no signing key id: %s", r.Stdout)
	}
	return m[1], nil
}

var signingKeyID = regexp.MustCompile(`signing ServiceAccount key id is (\S+)`)

// KubernetesNode is the arguments of gen kubernetes-node.
type KubernetesNode struct {
	CA, Name, Role, Address, Endpoint, PodNetwork, PodPrefix string
	// Peers are <address>=<pod-prefix> entries.
	Peers []string
	// MAC, Gateway, Search, Nameservers and ExtraNIC are the network-config's.
	MAC, Gateway, Search, ExtraNIC string
	Nameservers                    []string
	// LocalRegistry runs the registry service; RegistryCA and SandboxImage are what containerd trusts and pulls.
	LocalRegistry            bool
	RegistryCA, SandboxImage string
	Binaries                 map[string]string
	Disable                  []string
	Out, NetworkConfig       string
	// Storage are <name>=<label, /dev/path or serial:NUMBER> disks init formats and mounts, the first
	// holding the Node's local volumes, and BlockSerials the serial numbers of disks offered whole as
	// block volumes (gen kubernetes-node --storage, --block-serial; ADR 0083).
	Storage, BlockSerials []string
	// BootstrapTokens has a control-plane Node accept bootstrap tokens, and Token has a worker present
	// the one <ca-dir>/tokens/<name> holds (gen kubernetes-node --bootstrap-tokens, --token).
	BootstrapTokens, Token bool
}

// GenKubernetesNode has naructl write the Node's user-data to n.Out, and its network-config to
// n.NetworkConfig when that is set.
func (t Tool) GenKubernetesNode(n KubernetesNode) error {
	args := []string{"gen", "kubernetes-node", t.caDirFlag(), n.CA, "--name", n.Name, "--role", n.Role, "--address", n.Address,
		"--endpoint", n.Endpoint, "--pod-network", n.PodNetwork, "--pod-prefix", n.PodPrefix, "--out", n.Out}
	flag := func(name string, values ...string) {
		for _, v := range values {
			args = append(args, "--"+name, v)
		}
	}
	if n.LocalRegistry {
		args = append(args, "--local-registry")
	}
	if n.BootstrapTokens {
		args = append(args, "--bootstrap-tokens")
	}
	if n.Token {
		args = append(args, "--token")
	}
	flag("registry-ca", n.RegistryCA)
	flag("sandbox-image", n.SandboxImage)
	flag("peer", n.Peers...)
	if n.NetworkConfig != "" {
		flag("network-config", n.NetworkConfig)
		flag("mac", n.MAC)
		flag("gateway", n.Gateway)
		flag("nameserver", n.Nameservers...)
		if n.Search != "" {
			flag("search", n.Search)
		}
		if n.ExtraNIC != "" {
			flag("extra-nic", n.ExtraNIC)
		}
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
	flag("storage", n.Storage...)
	flag("block-serial", n.BlockSerials...)
	return t.run(args...)
}

// KubeTokenCreate and KubeTokenRevoke have naructl add or revoke a Kubernetes Node's bootstrap token
// on the running control-plane Node the client file config reaches.
func (t Tool) KubeTokenCreate(caDir, name, config string) error {
	return t.run("token", "create", "--kubernetes", t.caDirFlag(), caDir, "--name", name, "--config", config)
}

func (t Tool) KubeTokenRevoke(caDir, name, config string) error {
	return t.run("token", "revoke", "--kubernetes", t.caDirFlag(), caDir, "--name", name, "--config", config)
}

// KubeBootstrapRBAC is the manifest a cluster that takes bootstrap tokens needs, for kubectl apply.
func (t Tool) KubeBootstrapRBAC() (string, error) {
	b, err := t.output("gen", "kubernetes-bootstrap-rbac")
	return string(b), err
}
