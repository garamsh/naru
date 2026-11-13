package layout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
)

// File is one guest artifact: Path is where it is inside the image, SHA256 the digest of its bytes.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Manifest is guest.json, which image/guest/build.sh writes and the release carries as
// naru-guest-<version>.json (ADR 0010): the guest kernels and initrds a Node boots Machines from.
type Manifest struct {
	Kernels         map[string]File `json:"kernels"`
	DefaultKernel   string          `json:"default_kernel"`
	GuestInitrd     File            `json:"guest_initrd"`
	ConverterInitrd File            `json:"converter_initrd"`
}

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ReadManifest reads and checks the manifest at path.
func ReadManifest(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := m.Check(); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Check reports the first thing wrong with the manifest: a kernel name outside 1 to 63 characters
// of [a-z0-9-], a default that is not a declared kernel, or a file without a path or a digest.
func (m Manifest) Check() error {
	if len(m.Kernels) == 0 {
		return fmt.Errorf("no kernel is declared")
	}
	names := make([]string, 0, len(m.Kernels))
	for n := range m.Kernels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !namePattern.MatchString(n) {
			return fmt.Errorf("kernel name %q is not 1 to 63 characters of [a-z0-9-]", n)
		}
		if err := m.Kernels[n].check("kernel " + n); err != nil {
			return err
		}
	}
	if _, ok := m.Kernels[m.DefaultKernel]; !ok {
		return fmt.Errorf("default_kernel %q is not one of the declared kernels", m.DefaultKernel)
	}
	if err := m.GuestInitrd.check("guest_initrd"); err != nil {
		return err
	}
	return m.ConverterInitrd.check("converter_initrd")
}

func (f File) check(what string) error {
	if f.Path == "" || f.Path[0] != '/' {
		return fmt.Errorf("%s: path %q is not absolute", what, f.Path)
	}
	if !digestPattern.MatchString(f.SHA256) {
		return fmt.Errorf("%s: sha256 %q is not 64 lowercase hex digits", what, f.SHA256)
	}
	return nil
}

// mvmdJSON is mvmd's configuration for the Node: the Firecracker driver on the guest files of the
// image and every declared kernel, the registry's authority reached through the service's
// SSL_CERT_FILE, supervisors outside mvmd's cgroup, and a Machine network of its own per Node.
func (n Node) mvmdJSON() ([]byte, error) {
	if !digestPattern.MatchString(n.FirecrackerSHA256) {
		return nil, fmt.Errorf("the Firecracker digest %q is not 64 lowercase hex digits", n.FirecrackerSHA256)
	}
	g := n.Guest
	// The converter has one kernel of its own, which is the default.
	kernel := g.Kernels[g.DefaultKernel]
	kernels := make(map[string]any, len(g.Kernels))
	for name, k := range g.Kernels {
		kernels[name] = map[string]any{"path": k.Path, "sha256": k.SHA256}
	}
	prefix := n.MachinePrefix
	first := prefix.Masked().Addr().Next().String()
	network := map[string]any{
		"ipv4_prefix":  prefix.String(),
		"ipv4_gateway": first,
		"resolvers":    []string{first},
	}
	if len(n.Peers) > 0 {
		// A Machine's traffic to another Node's Machines keeps its source address, so the peer's
		// Machines see who sent it (mvmd ADR 0049).
		peers := make([]string, 0, len(n.Peers))
		for _, p := range n.Peers {
			peers = append(peers, p.MachinePrefix.String())
		}
		network["no_masquerade"] = peers
	}
	return json.MarshalIndent(map[string]any{
		"socket":    map[string]any{"path": MvmdSocket, "mode": "0660"},
		"state_dir": DataMount + "/mvmd",
		"driver":    "firecracker",
		"disk":      map[string]any{"mkfs": MkfsPath},
		"firecracker": map[string]any{
			"binary": "/bin/firecracker", "binary_sha256": n.FirecrackerSHA256,
			"kernels": kernels, "default_kernel": g.DefaultKernel,
			"initrd": g.GuestInitrd.Path, "initrd_sha256": g.GuestInitrd.SHA256,
		},
		"conversion": map[string]any{
			"kernel": kernel.Path, "kernel_sha256": kernel.SHA256,
			"initrd": g.ConverterInitrd.Path, "initrd_sha256": g.ConverterInitrd.SHA256,
			"converter": "naru-converter",
			"vcpus":     2, "memory_bytes": 512 << 20,
			"output_bytes": 256 << 20, "name_set_bytes": 1 << 20,
			"limit": "5m",
		},
		// supervision.dir is left out, so it is "machines" under state_dir on the data disk:
		// the state directory and the run directories stay on one file system (mvmd ADR 0015).
		"supervision":   map[string]any{"binary": "/bin/mvmd-supervisor", "cgroup": SupervisorCgroup},
		"vmm_uid_range": map[string]any{"first": 100000, "count": 1000},
		"network":       network,
		"pull_timeout":  "4m",
		"boot_timeout":  "2m",
		"call_timeout":  "5m",
	}, "", "  ")
}
