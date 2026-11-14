package api

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// Limits on one request.
const (
	maxDisks = 16
	maxRules = 64
	maxPorts = 16
)

var (
	dnsLabel    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	imagePieces = regexp.MustCompile(`^(?:([a-z0-9][a-z0-9._:/-]*)@)?(sha256:[0-9a-f]{64})$`)
)

// plan is a validated request: what to pull and what to create.
type plan struct {
	repository string
	create     mvmd.Create
	disks      []hostapi.DiskSpec
	// userData is the seed's user-data; only the OS form has one.
	userData []byte
}

// diskID is the mvmd identifier of a microVM's disk. A name is a DNS label, which has no '.', so
// the identifier cannot be another microVM's name or its root disk.
func diskID(name, disk string) string { return name + "." + disk }

// validate returns the plan for r, or every field that is wrong with it.
func validate(r hostapi.CreateRequest) (plan, []string, string) {
	if isOSForm(r) {
		return validateOS(r)
	}
	var fields []string
	bad := func(f string) { fields = append(fields, f) }
	if !dnsLabel.MatchString(r.Name) {
		bad("name")
	}
	m := imagePieces.FindStringSubmatch(r.Image)
	if m == nil {
		bad("image")
	}
	if r.CPU == 0 {
		bad("cpu")
	}
	if r.Memory == 0 {
		bad("memory")
	}
	if r.RootDiskBytes == 0 {
		bad("rootDiskBytes")
	}
	if len(r.Disks) > maxDisks {
		bad("disks")
	}
	seen := map[string]bool{}
	for i, d := range r.Disks {
		if !dnsLabel.MatchString(d.Name) || seen[d.Name] || d.SizeBytes == 0 {
			bad(fmt.Sprintf("disks[%d]", i))
		}
		seen[d.Name] = true
	}
	fields = append(fields, checkRules("networkPolicy.egress", r.NetworkPolicy.Egress)...)
	fields = append(fields, checkRules("networkPolicy.ingress", r.NetworkPolicy.Ingress)...)
	if len(fields) > 0 {
		return plan{}, fields, "the request is invalid"
	}
	// The pinned mvmd's CreateMachine has no environment or command for a Machine, so a request
	// that carries one is refused whole: it would otherwise be accepted and do nothing.
	var unsupported []string
	if len(r.Env) > 0 {
		unsupported = append(unsupported, "env")
	}
	if len(r.Command) > 0 {
		unsupported = append(unsupported, "command")
	}
	if len(unsupported) > 0 {
		return plan{}, unsupported, "this host's mvmd (runtime API v1alpha2) gives a Machine no environment and no command: the image's own entrypoint runs"
	}
	p := plan{repository: m[1], disks: slices.Clone(r.Disks), create: mvmd.Create{
		ID: r.Name, ImageDigest: m[2], CPU: r.CPU, MemoryBytes: r.Memory, RootDiskBytes: r.RootDiskBytes, Policy: r.NetworkPolicy,
		RestartOnBoot: restartOnBoot(r.RestartOnBoot, false), Fenced: r.Fenced,
	}}
	for _, d := range r.Disks {
		p.create.Disks = append(p.create.Disks, mvmd.Attachment{ID: diskID(r.Name, d.Name), ReadOnly: d.ReadOnly})
	}
	return p, nil, ""
}

// restartOnBoot is the request's choice, or the form's default when it made none.
func restartOnBoot(chosen *bool, formDefault bool) bool {
	if chosen == nil {
		return formDefault
	}
	return *chosen
}

var protocols = []string{"tcp", "udp", "icmp", "icmpv6", "any"}

func checkRules(field string, rules []mvmd.Rule) []string {
	var bad []string
	if len(rules) > maxRules {
		bad = append(bad, field)
	}
	for i, r := range rules {
		ok := slices.Contains(protocols, r.Protocol)
		if p, err := netip.ParsePrefix(r.Prefix); err != nil || p != p.Masked() {
			ok = false
		}
		if len(r.Ports) > maxPorts || len(r.Ports) > 0 && r.Protocol != "tcp" && r.Protocol != "udp" {
			ok = false
		}
		for _, p := range r.Ports {
			if p.First == 0 || p.Last > 65535 || p.First > p.Last {
				ok = false
			}
		}
		if !ok {
			bad = append(bad, fmt.Sprintf("%s[%d]", field, i))
		}
	}
	return bad
}

// diskName is a disk's name as the host API shows it: the identifier without the microVM's prefix.
func diskName(machine, id string) string { return strings.TrimPrefix(id, machine+".") }
