package naructl

import (
	"encoding/json"
	"fmt"
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
	args := []string{"gen", "kubernetes", t.caDirFlag(), caDir, "--endpoint", endpoint}
	if adminAddress != "" {
		args = append(args, "--admin-address", adminAddress)
	}
	for _, name := range disableAddons {
		args = append(args, "--disable-addon", name)
	}
	return t.run(args...)
}

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
