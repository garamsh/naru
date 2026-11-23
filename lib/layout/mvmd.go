package layout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/garamsh/naru/lib/wire"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// guestRef is what stands for a value of the booted image's guest.json in the document, until the
// document is a template: the template action init renders it with (config.File.Render).
type guestRef struct{ sentinel, action string }

var (
	guestKernels       = guestRef{"@guest:kernels@", "{{json .kernels}}"}
	guestDefaultKernel = guestRef{"@guest:default_kernel@", "{{json .default_kernel}}"}
	guestInitrdPath    = guestRef{"@guest:guest_initrd.path@", "{{json .guest_initrd.path}}"}
	guestInitrdSHA     = guestRef{"@guest:guest_initrd.sha256@", "{{json .guest_initrd.sha256}}"}
	guestConvPath      = guestRef{"@guest:converter_initrd.path@", "{{json .converter_initrd.path}}"}
	guestConvSHA       = guestRef{"@guest:converter_initrd.sha256@", "{{json .converter_initrd.sha256}}"}
	// The converter has one kernel of its own, which is the default.
	guestConvKernelPath = guestRef{"@guest:converter_kernel.path@", "{{json (index .kernels .default_kernel).path}}"}
	guestConvKernelSHA  = guestRef{"@guest:converter_kernel.sha256@", "{{json (index .kernels .default_kernel).sha256}}"}
	guestRefs           = []guestRef{guestKernels, guestDefaultKernel, guestInitrdPath, guestInitrdSHA, guestConvPath, guestConvSHA, guestConvKernelPath, guestConvKernelSHA}
)

// asTemplate turns the marshaled document into the guest template init renders: every `{{` the
// operator's own strings hold is written as the text it is, and each sentinel, which stands for a
// whole JSON value, becomes its action.
func asTemplate(doc []byte) []byte {
	doc = bytes.ReplaceAll(doc, []byte("{{"), []byte(`{{"{{"}}`))
	for _, r := range guestRefs {
		doc = bytes.ReplaceAll(doc, []byte(`"`+r.sentinel+`"`), []byte(r.action))
	}
	return doc
}

// mvmdJSON is mvmd's configuration for the Node, as the template init renders (spec.files
// `template: guest`): the Firecracker driver on the guest files of the image and every declared kernel,
// the registry section for the files init writes from spec.registries, supervisors outside mvmd's
// cgroup, and a Machine network of its own per Node. The guest kernels, initrds and their digests are
// not in it: they are the booted image's guest.json (ADR 0010), which init reads at each write, so an
// upgrade that changes them changes the file with no edit of the configuration (ADR 0106).
func (n Node) mvmdJSON() ([]byte, error) {
	if !digestPattern.MatchString(n.FirecrackerSHA256) {
		return nil, fmt.Errorf("the Firecracker digest %q is not 64 lowercase hex digits", n.FirecrackerSHA256)
	}
	prefix := n.MachinePrefix
	first := prefix.Masked().Addr().Next().String()
	network := map[string]any{
		"ipv4_prefix":  prefix.String(),
		"ipv4_gateway": first,
		"resolvers":    []string{first},
	}
	if len(n.Peers)+len(n.NoMasquerade) > 0 {
		// A Machine's traffic to another Node's Machines keeps its source address, so the peer's
		// Machines see who sent it (mvmd ADR 0049).
		keep := make([]string, 0, len(n.Peers)+len(n.NoMasquerade))
		for _, p := range n.Peers {
			keep = append(keep, p.MachinePrefix.String())
		}
		for _, p := range n.NoMasquerade {
			keep = append(keep, p.String())
		}
		network["no_masquerade"] = keep
	}
	registry := map[string]any{"credentials_file": RegistryFilesDir + "/registry/credentials.json"}
	helpers := map[string]string{}
	for _, r := range n.Registries {
		if r.CA != "" {
			registry["ca_file"] = RegistryFilesDir + "/registry/ca.pem"
		}
		if r.Helper != "" {
			helpers[r.Host] = r.Helper
		}
	}
	if len(helpers) > 0 {
		registry["credential_helpers"] = helpers
	}
	// Without storage supervision.dir is left out, so it is "machines" under state_dir on the data
	// disk. With it, the run directories sit on the storage mount that init links disks/ and
	// snapshots/ into, so mvmd's rename of a snapshot stays on one file system (ADR 0073).
	supervision := map[string]any{"binary": "/bin/mvmd-supervisor", "cgroup": SupervisorCgroup}
	if len(n.Storage) > 0 {
		supervision["dir"] = wire.MvmdStorageMount + "/machines"
	}
	doc, err := json.MarshalIndent(map[string]any{
		"registry":  registry,
		"socket":    map[string]any{"path": MvmdSocket, "mode": "0660"},
		"state_dir": wire.MvmdStateDir,
		"driver":    "firecracker",
		"disk":      map[string]any{"mkfs": wire.MkfsPath},
		"firecracker": map[string]any{
			"binary": "/bin/firecracker", "binary_sha256": n.FirecrackerSHA256,
			"kernels": guestKernels.sentinel, "default_kernel": guestDefaultKernel.sentinel,
			"initrd": guestInitrdPath.sentinel, "initrd_sha256": guestInitrdSHA.sentinel,
		},
		"conversion": map[string]any{
			"kernel": guestConvKernelPath.sentinel, "kernel_sha256": guestConvKernelSHA.sentinel,
			"initrd": guestConvPath.sentinel, "initrd_sha256": guestConvSHA.sentinel,
			"converter": "naru-converter",
			"vcpus":     2, "memory_bytes": 512 << 20,
			"output_bytes": 256 << 20, "name_set_bytes": 1 << 20,
			"limit": "5m",
		},
		"supervision":   supervision,
		"vmm_uid_range": map[string]any{"first": 100000, "count": 1000},
		"network":       network,
		"pull_timeout":  "4m",
		"boot_timeout":  "2m",
		"call_timeout":  "5m",
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return asTemplate(doc), nil
}
