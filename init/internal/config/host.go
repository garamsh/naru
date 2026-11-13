package config

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// HostFile is the file init writes from spec.host, for naru-hostd to read on each request
// (docs/architecture/adr/0028-naru-hostd-serves-the-host-api-and-keeps-mvmds-fencing-deadline.md).
// It is a file of spec.files that Parse derives, so a change to spec.host reaches a running Node
// by the apply that writes any file, with no reboot and no restart.
const HostFile = "/run/naru-hostd/host.json"

// Host is where the Node is and how it is told apart: the region and labels naru-hostd reports.
type Host struct {
	// Region is a DNS label.
	Region string `yaml:"region"`
	// Labels are Kubernetes-style label pairs.
	Labels map[string]string `yaml:"labels,omitempty"`
}

var (
	labelValueRE = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`)
	labelNameRE  = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

const maxLabels = 64

func (v *validation) host(h *Host, files []File) {
	if h == nil {
		return
	}
	if !nameRE.MatchString(h.Region) {
		v.fail("spec.host.region", "must be lowercase letters, digits and '-', at most 63 characters, got %q", h.Region)
	}
	if len(h.Labels) > maxLabels {
		v.fail("spec.host.labels", "has %d labels; at most %d", len(h.Labels), maxLabels)
	}
	for k, val := range h.Labels {
		if !validLabelKey(k) {
			v.fail("spec.host.labels", "has the invalid key %q: an optional DNS subdomain prefix and '/', then a name of at most 63 characters", k)
		}
		if !labelValueRE.MatchString(val) {
			v.fail("spec.host.labels", "has the invalid value %q for %q: at most 63 letters, digits, '-', '_' and '.', beginning and ending with a letter or digit", val, k)
		}
	}
}

func validLabelKey(k string) bool {
	prefix, name, found := strings.Cut(k, "/")
	if !found {
		return labelNameRE.MatchString(k)
	}
	return len(prefix) <= 253 && dnsSubdomain.MatchString(prefix) && labelNameRE.MatchString(name)
}

// withHostFile returns files without any entry at HostFile, which is the derived file of a
// configuration that was stored and is parsed again, and with the one spec.host derives when it
// is set. The file therefore follows spec.host: an apply that removes spec.host does not keep it.
func withHostFile(files []File, h *Host) []File {
	out := slices.DeleteFunc(slices.Clone(files), func(f File) bool { return f.Path == HostFile })
	if h != nil {
		out = append(out, hostFile(*h))
	}
	return out
}

// hostFile returns the entry of spec.files that carries spec.host.
func hostFile(h Host) File {
	labels := h.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	// A map's JSON keys are sorted, so equal hosts give equal content and an apply of an
	// unchanged spec.host writes nothing.
	b, _ := json.Marshal(struct {
		Region string            `json:"region"`
		Labels map[string]string `json:"labels"`
	}{h.Region, labels})
	return File{Path: HostFile, Mode: "0644", Content: string(b) + "\n"}
}
