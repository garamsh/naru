package api

import (
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// Limits on one request. A Machine's files are mvmd's limit: 64 files and 1 MiB of content.
const (
	maxDisks        = 16
	maxRules        = 64
	maxPorts        = 16
	maxFiles        = 64
	maxFilesContent = 1 << 20
	maxFileMode     = 0o7777
)

var (
	dnsLabel    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	imagePieces = regexp.MustCompile(`^(?:([a-z0-9][a-z0-9._:/-]*)@)?(sha256:[0-9a-f]{64})$`)
)

// plan is a validated request: what to pull and what to create.
type plan struct {
	repository string
	create     mvmd.Create
	// noStart leaves the Machine created, as a fenced one is.
	noStart bool
	disks   []hostapi.DiskSpec
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
		if !dnsLabel.MatchString(d.Name) || seen[d.Name] || d.SizeBytes == 0 ||
			d.MountPath != "" && !mountable(d.MountPath) || d.FromSnapshot != "" && (d.Imported || !dnsLabel.MatchString(d.FromSnapshot)) {
			bad(fmt.Sprintf("disks[%d]", i))
		}
		seen[d.Name] = true
	}
	for name := range r.Env {
		if name == "" || strings.Contains(name, "=") {
			bad("env")
			break
		}
	}
	if r.WorkingDir != "" && !cleanAbsolute(r.WorkingDir) {
		bad("workingDir")
	}
	fields = append(fields, checkFiles(r.Files)...)
	fields = append(fields, checkRules("networkPolicy.egress", r.NetworkPolicy.Egress)...)
	fields = append(fields, checkRules("networkPolicy.ingress", r.NetworkPolicy.Ingress)...)
	if len(fields) > 0 {
		return plan{}, fields, "the request is invalid"
	}
	p := plan{repository: m[1], disks: slices.Clone(r.Disks), create: mvmd.Create{
		ID: r.Name, ImageDigest: m[2], CPU: r.CPU, MemoryBytes: r.Memory, RootDiskBytes: r.RootDiskBytes, Policy: r.NetworkPolicy,
		RestartOnBoot: restartOnBoot(r.RestartOnBoot, false), Fenced: r.Fenced,
		Command: r.Command, Args: r.Args, Env: r.Env, User: r.User, WorkingDir: r.WorkingDir, Files: r.Files,
	}}
	for _, d := range r.Disks {
		p.create.Disks = append(p.create.Disks, mvmd.Attachment{ID: diskID(r.Name, d.Name), ReadOnly: d.ReadOnly, MountPath: d.MountPath})
	}
	return p, nil, ""
}

// cleanAbsolute reports whether p is an absolute path with no ".", ".." or empty element and no
// trailing slash, as mvmd takes a guest path.
func cleanAbsolute(p string) bool { return path.IsAbs(p) && path.Clean(p) == p }

// mountable reports whether the guest may mount a disk at p: a clean absolute path other than the
// root, and outside the trees the guest's own system holds.
func mountable(p string) bool {
	if !cleanAbsolute(p) || p == "/" {
		return false
	}
	for _, sys := range []string{"/proc", "/sys", "/dev", "/run/mvmd"} {
		if p == sys || strings.HasPrefix(p, sys+"/") {
			return false
		}
	}
	return true
}

// checkFiles returns the fields of the files mvmd would refuse: more than maxFiles, more than
// maxFilesContent in all, a path that is not clean and absolute or given twice, a mode above 07777.
func checkFiles(files []hostapi.File) []string {
	var bad []string
	if len(files) > maxFiles {
		bad = append(bad, "files")
	}
	total, seen := 0, map[string]bool{}
	for i, f := range files {
		total += len(f.Content)
		if !cleanAbsolute(f.Path) || seen[f.Path] || f.Mode > maxFileMode {
			bad = append(bad, fmt.Sprintf("files[%d]", i))
		}
		seen[f.Path] = true
	}
	if total > maxFilesContent && !slices.Contains(bad, "files") {
		bad = append(bad, "files")
	}
	return bad
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
