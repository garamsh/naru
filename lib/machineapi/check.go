package machineapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"
)

// ErrInvalid is the refusal of a request a server cannot take as written. A FieldError wraps it.
var ErrInvalid = errors.New("invalid")

// FieldError is a refusal naming the field it is about, such as "spec.volumes[0].mountPath".
type FieldError struct {
	Field   string
	Problem string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Problem }

// Unwrap makes a FieldError match ErrInvalid.
func (e *FieldError) Unwrap() error { return ErrInvalid }

func invalid(field, format string, a ...any) error {
	return &FieldError{Field: field, Problem: fmt.Sprintf(format, a...)}
}

var (
	label    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	idForm   = regexp.MustCompile(`^[0-9a-f]{16}$`)
	imageRef = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)
)

// CheckName refuses a name that is not a DNS label: 1 to 63 lowercase letters, digits and "-",
// beginning and ending with a letter or a digit. Machines, snapshots, volumes and services are
// named so.
func CheckName(name string) error {
	if !label.MatchString(name) {
		return fmt.Errorf("%w: %q is not a DNS label", ErrInvalid, name)
	}
	return nil
}

// IsMachineID reports whether s has the form of a Machine's id: 16 lowercase hexadecimal digits.
func IsMachineID(s string) bool { return idForm.MatchString(s) }

// gib is the unit a GPU Machine's memory is a whole number of (ADR 0111 item 6).
const gib = 1 << 30

// CheckPath refuses a guest path that is not absolute and clean, is "/", or is under /proc, /sys,
// /dev or /run/mvmd, as mvmd refuses it.
func CheckPath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return fmt.Errorf("%w: %q is not an absolute, clean path below /", ErrInvalid, p)
	}
	for _, d := range []string{"/proc", "/sys", "/dev", "/run/mvmd"} {
		if p == d || strings.HasPrefix(p, d+"/") {
			return fmt.Errorf("%w: %q is under %s", ErrInvalid, p, d)
		}
	}
	return nil
}

// Check refuses an exec with an empty command or a timeout above MaxExecSeconds, with a FieldError.
func (r ExecRequest) Check() error {
	switch {
	case len(r.Command) == 0 || r.Command[0] == "":
		return invalid("command", "is empty")
	case r.TimeoutSeconds > MaxExecSeconds:
		return invalid("timeoutSeconds", "is %d; at most %d", r.TimeoutSeconds, MaxExecSeconds)
	}
	return nil
}

// Check refuses a stop whose grace is above MaxGraceSeconds, with a FieldError.
func (r StopRequest) Check() error {
	if r.GraceSeconds > MaxGraceSeconds {
		return invalid("graceSeconds", "is %d; at most %d", r.GraceSeconds, MaxGraceSeconds)
	}
	return nil
}

// Check refuses a spec a create cannot take, with a FieldError naming the first field at fault.
func (s Spec) Check() error {
	switch {
	case !imageRef.MatchString(s.Image):
		return invalid("spec.image", "is not repository@sha256:<64 hex digits>")
	case s.CPU == 0:
		return invalid("spec.cpu", "must be at least 1")
	case s.MemoryBytes == 0:
		return invalid("spec.memoryBytes", "must be above 0")
	case s.RootDiskBytes == 0:
		return invalid("spec.rootDiskBytes", "must be above 0")
	case s.State != "" && s.State != StateRunning && s.State != StateStopped:
		return invalid("spec.state", "is not %q or %q", StateRunning, StateStopped)
	case s.FromSnapshot != nil && CheckName(s.FromSnapshot.Snapshot) != nil:
		return invalid("spec.fromSnapshot.snapshot", "is not a DNS label")
	}
	for _, check := range []func() error{s.checkVolumes, s.checkRun, s.checkServices, s.checkPolicy, s.checkShape} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func (s Spec) checkVolumes() error {
	names, mounts := map[string]bool{}, map[string]bool{}
	for i, v := range s.Volumes {
		f := fmt.Sprintf("spec.volumes[%d]", i)
		switch {
		case CheckName(v.Name) != nil || names[v.Name]:
			return invalid(f+".name", "is not a DNS label unique among the volumes")
		case v.SizeBytes == 0:
			return invalid(f+".sizeBytes", "must be above 0")
		case CheckPath(v.MountPath) != nil || mounts[v.MountPath]:
			return invalid(f+".mountPath", "is not a clean absolute path unique among the volumes, outside /proc, /sys, /dev and /run/mvmd")
		}
		names[v.Name], mounts[v.MountPath] = true, true
	}
	return nil
}

func (s Spec) checkRun() error {
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		if k == "" || strings.Contains(k, "=") {
			return invalid("spec.env", "the name %q is empty or holds \"=\"", k)
		}
	}
	if s.WorkingDir != "" && (!path.IsAbs(s.WorkingDir) || path.Clean(s.WorkingDir) != s.WorkingDir) {
		return invalid("spec.workingDir", "is not an absolute, clean path")
	}
	if len(s.Files) > 64 {
		return invalid("spec.files", "holds more than 64 files")
	}
	seen, total := map[string]bool{}, 0
	for i, f := range s.Files {
		field := fmt.Sprintf("spec.files[%d]", i)
		switch {
		case CheckPath(f.Path) != nil || seen[f.Path]:
			return invalid(field+".path", "is not a clean absolute path unique among the files, outside /proc, /sys, /dev and /run/mvmd")
		case f.Mode > 0o7777:
			return invalid(field+".mode", "is above 07777")
		case f.SizeBytes != 0:
			return invalid(field+".sizeBytes", "is answered by a read and not taken by a create")
		}
		seen[f.Path] = true
		total += len(f.Content)
	}
	if total > 1<<20 {
		return invalid("spec.files", "hold more than 1 MiB of content")
	}
	return nil
}

func (s Spec) checkServices() error {
	seen := map[string]bool{}
	for i, v := range s.Services {
		f := fmt.Sprintf("spec.services[%d]", i)
		switch {
		case CheckName(v.Name) != nil || strings.Contains(v.Name, "--") || seen[v.Name]:
			return invalid(f+".name", "is not a DNS label without \"--\" unique among the services")
		case v.Port == 0:
			return invalid(f+".port", "must be 1 to 65535")
		case v.Protocol != ServiceHTTP && v.Protocol != ServiceTCP:
			return invalid(f+".protocol", "is not %q or %q", ServiceHTTP, ServiceTCP)
		case v.ProxyProtocol != "" && v.ProxyProtocol != ProxyProtocolV2:
			return invalid(f+".proxyProtocol", "is not %q", ProxyProtocolV2)
		}
		seen[v.Name] = true
	}
	return nil
}

func (s Spec) checkPolicy() error {
	for i, r := range s.NetworkPolicy.Egress {
		f := fmt.Sprintf("spec.networkPolicy.egress[%d]", i)
		p, err := netip.ParsePrefix(r.Prefix)
		switch {
		case err != nil || p.Masked() != p:
			return invalid(f+".prefix", "is not a prefix in canonical form")
		case !slices.Contains([]string{"tcp", "udp", "icmp", "icmpv6", "any"}, r.Protocol):
			return invalid(f+".protocol", "is not tcp, udp, icmp, icmpv6 or any")
		case r.Protocol == "icmp" && !p.Addr().Is4(), r.Protocol == "icmpv6" && !p.Addr().Is6():
			return invalid(f+".protocol", "does not match the prefix's family")
		case len(r.Ports) > 0 && r.Protocol != "tcp" && r.Protocol != "udp":
			return invalid(f+".ports", "are taken only by tcp and udp")
		}
		for j, pr := range r.Ports {
			if pr.First == 0 || pr.Last < pr.First {
				return invalid(fmt.Sprintf("%s.ports[%d]", f, j), "is not a range of 1 to 65535, first to last")
			}
		}
	}
	return nil
}

func (s Spec) checkShape() error {
	g := s.Shape.GPU
	switch {
	case g == nil:
		return nil
	case g.Model == "" || g.Count == 0:
		return invalid("spec.shape.gpu", "needs a model and a count of at least 1")
	case uint64(s.CPU) < 8*uint64(g.Count):
		return invalid("spec.cpu", "must be at least %d, 8 per GPU", 8*uint64(g.Count))
	case s.MemoryBytes%gib != 0:
		return invalid("spec.memoryBytes", "must be a whole number of GiB on a GPU Machine")
	}
	return nil
}

// SameSpec reports whether a and b are the same spec for create's idempotency: State aside, and an
// empty list or map the same as an absent one.
// The encoding omits an empty list or map and orders a map's keys, so equal encodings are the same
// spec.
func SameSpec(a, b Spec) bool {
	a.State, b.State = "", ""
	ja, ea := json.Marshal(a)
	jb, eb := json.Marshal(b)
	return ea == nil && eb == nil && string(ja) == string(jb)
}

// Redact returns s as a read answers it: every env value Redacted and every file's content
// replaced by its size.
func (s Spec) Redact() Spec {
	if s.Env != nil {
		env := make(map[string]string, len(s.Env))
		for k := range s.Env {
			env[k] = Redacted
		}
		s.Env = env
	}
	if s.Files != nil {
		files := make([]File, len(s.Files))
		for i, f := range s.Files {
			files[i] = File{Path: f.Path, Mode: f.Mode, SizeBytes: uint64(len(f.Content))}
		}
		s.Files = files
	}
	return s
}

// ErrVolumesMismatch is the refusal of a restore whose volumes are not the snapshot's.
var ErrVolumesMismatch = errors.New("the volumes are not the snapshot's")

// MatchVolumes refuses, with ErrVolumesMismatch, a restore whose volumes are not the snapshot's by
// name and size: none more, none fewer and none of another size.
func MatchVolumes(snapshot, spec []Volume) error {
	size := map[string]uint64{}
	for _, v := range snapshot {
		size[v.Name] = v.SizeBytes
	}
	for _, v := range spec {
		if got, ok := size[v.Name]; !ok || got != v.SizeBytes {
			return fmt.Errorf("%w: the volume %s of %d bytes is not the snapshot's", ErrVolumesMismatch, v.Name, v.SizeBytes)
		}
		delete(size, v.Name)
	}
	for _, n := range slices.Sorted(maps.Keys(size)) {
		return fmt.Errorf("%w: the snapshot's volume %s is not in the spec", ErrVolumesMismatch, n)
	}
	return nil
}
