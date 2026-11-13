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
func (t Tool) GenKubernetes(caDir, endpoint, adminAddress string) error {
	args := []string{"gen", "kubernetes", "--ca", caDir, "--endpoint", endpoint}
	if adminAddress != "" {
		args = append(args, "--admin-address", adminAddress)
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
}

// GenKubernetesNode has naructl write the Node's user-data to n.Out, and its network-config to
// n.NetworkConfig when that is set.
func (t Tool) GenKubernetesNode(n KubernetesNode) error {
	args := []string{"gen", "kubernetes-node", "--ca", n.CA, "--name", n.Name, "--role", n.Role, "--address", n.Address,
		"--endpoint", n.Endpoint, "--pod-network", n.PodNetwork, "--pod-prefix", n.PodPrefix, "--out", n.Out}
	flag := func(name string, values ...string) {
		for _, v := range values {
			args = append(args, "--"+name, v)
		}
	}
	if n.LocalRegistry {
		args = append(args, "--local-registry")
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
