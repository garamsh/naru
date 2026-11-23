package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"slices"

	"go.yaml.in/yaml/v4"
)

// SeedPodPrefixKey is the key of the NoCloud meta-data that carries the Pod prefix the host routed to
// the microVM. The meta-data is the one place a member's own values are written: its user-data is
// the same for every member of a group.
const SeedPodPrefixKey = "naru-pod-prefix"

// Node is what makes the Node a member of a Pod network: the prefix its Pods take addresses from.
type Node struct {
	// PodPrefix is the IPv4 prefix the Node's Pods take addresses from. The seed's meta-data key
	// naru-pod-prefix sets it and overrides the user-data's.
	PodPrefix netip.Prefix `yaml:"podPrefix,omitempty"`
	// CNIConf is the absolute path at which init writes the bridge CNI configuration for PodPrefix, as
	// a file of spec.files. The file follows PodPrefix: with none set there is no file.
	CNIConf string `yaml:"cniConf,omitempty"`
}

func (v *validation) node(n *Node) {
	if n == nil {
		return
	}
	if n.PodPrefix.IsValid() && (!n.PodPrefix.Addr().Is4() || n.PodPrefix != n.PodPrefix.Masked()) {
		v.fail("spec.node.podPrefix", "must be an IPv4 prefix with no host bits set, got %s", n.PodPrefix)
	}
	if n.CNIConf != "" && (!path.IsAbs(n.CNIConf) || path.Clean(n.CNIConf) != n.CNIConf) {
		v.fail("spec.node.cniConf", "must be a clean absolute path, got %q", n.CNIConf)
	}
}

// withCNIFile returns files with the CNI configuration spec.node derives: the entry at n.CNIConf is
// replaced, or removed when n names no Pod prefix, so a stored configuration that is parsed again
// keeps one entry and an apply that changes the prefix changes the file.
func withCNIFile(files []File, n *Node) []File {
	if n == nil || n.CNIConf == "" {
		return files
	}
	out := slices.DeleteFunc(slices.Clone(files), func(f File) bool { return f.Path == n.CNIConf })
	if n.PodPrefix.IsValid() {
		out = append(out, File{Path: n.CNIConf, Mode: "0644", Content: CNIConflist(n.PodPrefix)})
	}
	return out
}

// CNIConflist is the bridge CNI configuration of a Node whose Pods take addresses from prefix.
func CNIConflist(prefix netip.Prefix) string {
	b, _ := json.MarshalIndent(map[string]any{
		"cniVersion": "1.0.0", "name": "naru",
		"plugins": []any{map[string]any{
			"type": "bridge", "bridge": "cni0", "isGateway": true, "isDefaultGateway": true, "ipMasq": false, "hairpinMode": true,
			"ipam": map[string]any{"type": "host-local", "ranges": [][]map[string]string{{{"subnet": prefix.String()}}}},
		}},
	}, "", "  ")
	return string(b) + "\n"
}

// ApplySeedMeta returns cfg with the values the seed's meta-data carries for this member: its Pod
// prefix, which replaces spec.node.podPrefix. Meta-data with none of those keys, or none, changes
// nothing; keys init does not know (instance-id, local-hostname) are ignored.
func ApplySeedMeta(cfg NodeConfig, meta []byte) (NodeConfig, error) {
	if len(bytes.TrimSpace(meta)) == 0 {
		return cfg, nil
	}
	var m struct {
		PodPrefix string `yaml:"naru-pod-prefix"`
	}
	if err := yaml.Load(meta, &m); err != nil {
		return cfg, fmt.Errorf("parse the seed's meta-data: %w", err)
	}
	if m.PodPrefix == "" {
		return cfg, nil
	}
	p, err := netip.ParsePrefix(m.PodPrefix)
	if err != nil {
		return cfg, fmt.Errorf("the seed's meta-data %s is not a prefix: %w", SeedPodPrefixKey, err)
	}
	n := Node{}
	if cfg.Spec.Node != nil {
		n = *cfg.Spec.Node
	}
	n.PodPrefix = p
	var v validation
	v.node(&n)
	if len(v.errs) > 0 {
		return cfg, fmt.Errorf("the seed's meta-data %s: %w", SeedPodPrefixKey, errors.Join(v.errs...))
	}
	cfg.Spec.Node = &n
	cfg.Spec.Files = withCNIFile(cfg.Spec.Files, cfg.Spec.Node)
	return cfg, nil
}
