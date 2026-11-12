package config

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v4"
)

// DetachedName is the cgroup init keeps for processes a service places
// outside its own, so no service may take it as a name.
const DetachedName = "detached"

// maxLabelLen is the length of an ext4 volume label.
const maxLabelLen = 16

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Parse reads a naru.yaml and returns the configuration it holds, or an error
// joining one *FieldError for each field that is unknown, mistyped or invalid.
func Parse(data []byte) (NodeConfig, error) {
	var tree yaml.Node
	if err := yaml.Load(data, &tree); err != nil {
		return NodeConfig{}, fmt.Errorf("parse %s: %w", FileName, err)
	}
	var cfg NodeConfig
	if err := yaml.Load(data, &cfg, yaml.WithKnownFields()); err != nil {
		var loadErrs *yaml.LoadErrors
		if !errors.As(err, &loadErrs) {
			return NodeConfig{}, fmt.Errorf("parse %s: %w", FileName, err)
		}
		return NodeConfig{}, fieldErrors(&tree, loadErrs)
	}
	if err := validate(cfg); err != nil {
		return NodeConfig{}, err
	}
	return cfg, nil
}

// fieldErrors names, for each decode error, the field on the line it points at.
func fieldErrors(tree *yaml.Node, errs *yaml.LoadErrors) error {
	var all []error
	for _, e := range errs.Errors {
		field := fieldAtLine(tree, e.Mark.Line, "")
		if field == "" {
			field = fmt.Sprintf("line %d", e.Mark.Line)
		}
		all = append(all, &FieldError{Field: field, Message: e.Message})
	}
	return errors.Join(all...)
}

// fieldAtLine returns the path of the first key, or sequence item, that starts on line.
func fieldAtLine(n *yaml.Node, line int, prefix string) string {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			if p := fieldAtLine(c, line, prefix); p != "" {
				return p
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := prefix + n.Content[i].Value
			if prefix != "" {
				key = prefix + "." + n.Content[i].Value
			}
			if n.Content[i].Line == line {
				return key
			}
			if p := fieldAtLine(n.Content[i+1], line, key); p != "" {
				return p
			}
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			item := fmt.Sprintf("%s[%d]", prefix, i)
			if p := fieldAtLine(c, line, item); p != "" {
				return p
			}
			if c.Line == line {
				return item
			}
		}
	}
	return ""
}

func validate(cfg NodeConfig) error {
	var v validation
	if cfg.APIVersion != APIVersion {
		v.fail("apiVersion", "must be %q, got %q", APIVersion, cfg.APIVersion)
	}
	if cfg.Kind != Kind {
		v.fail("kind", "must be %q, got %q", Kind, cfg.Kind)
	}
	if !nameRE.MatchString(cfg.Metadata.Name) {
		v.fail("metadata.name", "must be lowercase letters, digits and '-', at most 63 characters, got %q", cfg.Metadata.Name)
	}
	v.services(cfg.Spec.Services)
	v.dataDisk(cfg.Spec.DataDisk)
	v.network(cfg.Spec.Network)
	v.files(cfg.Spec.Files)
	v.pki(cfg.Spec.PKI, cfg.Spec.Files, cfg.Spec.Services)
	return errors.Join(v.errs...)
}

type validation struct{ errs []error }

func (v *validation) fail(field, format string, args ...any) {
	v.errs = append(v.errs, &FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (v *validation) services(services []Service) {
	seen := map[string]bool{}
	for i, s := range services {
		field := fmt.Sprintf("spec.services[%d]", i)
		switch {
		case !nameRE.MatchString(s.Name):
			v.fail(field+".name", "must be lowercase letters, digits and '-', at most 63 characters, got %q", s.Name)
		case s.Name == DetachedName:
			v.fail(field+".name", "%q is reserved", s.Name)
		case seen[s.Name]:
			v.fail(field+".name", "%q is used by an earlier service", s.Name)
		}
		seen[s.Name] = true
		if !isCleanAbs(s.Binary) {
			v.fail(field+".binary", "must be a clean absolute path, got %q", s.Binary)
		}
		for k := range s.Env {
			if k == "" || strings.ContainsAny(k, "=\x00") {
				v.fail(field+".env", "has the invalid variable name %q", k)
			}
		}
	}
}

func (v *validation) dataDisk(d DataDisk) {
	switch {
	case d.Device == "" && d.Label == "":
		v.fail("spec.dataDisk", "must name the disk by device or by label")
	case d.Device != "" && d.Label != "":
		v.fail("spec.dataDisk", "names the disk by both device and label; give one")
	case d.Device != "" && !isCleanAbs(d.Device):
		v.fail("spec.dataDisk.device", "must be a clean absolute path, got %q", d.Device)
	case len(d.Label) > maxLabelLen:
		v.fail("spec.dataDisk.label", "must be at most %d bytes, got %d", maxLabelLen, len(d.Label))
	}
	if !isCleanAbs(d.MountPoint) || d.MountPoint == "/" {
		v.fail("spec.dataDisk.mountPoint", "must be a clean absolute path other than /, got %q", d.MountPoint)
	}
}

func (v *validation) network(n Network) {
	seen := map[Family]bool{}
	for i, f := range n.Forwarding {
		field := fmt.Sprintf("spec.network.forwarding[%d]", i)
		switch {
		case f != IPv4 && f != IPv6:
			v.fail(field, "must be %q or %q, got %q", IPv4, IPv6, f)
		case seen[f]:
			v.fail(field, "%q is listed twice", f)
		}
		seen[f] = true
	}
}

func (v *validation) files(files []File) {
	for i, f := range files {
		field := fmt.Sprintf("spec.files[%d]", i)
		switch {
		case !isCleanAbs(f.Path) || !strings.HasPrefix(f.Path, FilesRoot+"/"):
			v.fail(field+".path", "must be a clean absolute path under %s, got %q", FilesRoot, f.Path)
		case f.Path == ReservedDir || strings.HasPrefix(f.Path, ReservedDir+"/"):
			v.fail(field+".path", "%s is init's own directory, got %q", ReservedDir, f.Path)
		}
		if f.Mode != "" {
			if m, err := strconv.ParseUint(f.Mode, 8, 32); err != nil || m > 0o777 {
				v.fail(field+".mode", "must be octal permission bits up to 0777, got %q", f.Mode)
			}
		}
		for j, earlier := range files[:i] {
			if f.Path == earlier.Path || strings.HasPrefix(f.Path, earlier.Path+"/") || strings.HasPrefix(earlier.Path, f.Path+"/") {
				v.fail(field+".path", "%q conflicts with spec.files[%d], %q", f.Path, j, earlier.Path)
			}
		}
	}
}

func isCleanAbs(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p
}
