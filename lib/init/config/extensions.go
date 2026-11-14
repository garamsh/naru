package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	// ExtensionsRoot is the directory a system extension is mounted under, as ExtensionsRoot/<name>.
	ExtensionsRoot = "/usr/lib/naru/extensions"
	// OrchExtension is the extension that carries naru-orchd and naru-signer.
	OrchExtension = "naru-orch"
)

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Extension names a system extension image by the SHA-256 of the file.
type Extension struct {
	Name   string `yaml:"name"`
	SHA256 string `yaml:"sha256"`
}

// Dir returns the directory the extension is mounted on.
func (e Extension) Dir() string { return ExtensionsRoot + "/" + e.Name }

// Extension returns the extension named name, and false when the configuration lists none.
func (s Spec) Extension(name string) (Extension, bool) {
	i := slices.IndexFunc(s.Extensions, func(e Extension) bool { return e.Name == name })
	if i < 0 {
		return Extension{}, false
	}
	return s.Extensions[i], true
}

// ExtensionOf returns the name of the extension svc runs from, and "" when its binary is not
// under ExtensionsRoot.
func ExtensionOf(svc Service) string {
	rest, ok := strings.CutPrefix(svc.Binary, ExtensionsRoot+"/")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	return name
}

// extensions checks spec.extensions, and that spec.orch and spec.signer, and every service that
// runs from an extension, have the entry for it.
func (v *validation) extensions(spec Spec) {
	names := map[string]bool{}
	digests := map[string]bool{}
	for i, e := range spec.Extensions {
		field := fmt.Sprintf("spec.extensions[%d]", i)
		if !nameRE.MatchString(e.Name) {
			v.fail(field+".name", "must be lowercase letters, digits and '-', at most 63 characters, got %q", e.Name)
		}
		if !sha256RE.MatchString(e.SHA256) {
			v.fail(field+".sha256", "must be 64 lowercase hex digits, the SHA-256 of the image, got %q", e.SHA256)
		}
		if names[e.Name] {
			v.fail(field+".name", "%q is listed twice", e.Name)
		}
		if digests[e.SHA256] {
			v.fail(field+".sha256", "%q is listed twice", e.SHA256)
		}
		names[e.Name], digests[e.SHA256] = true, true
	}
	if (spec.Orch != nil || spec.Signer != nil) && !names[OrchExtension] {
		section := "spec.orch"
		if spec.Orch == nil {
			section = "spec.signer"
		}
		v.fail("spec.extensions", "needs an entry named %s: %s runs naru-orchd and naru-signer from it", OrchExtension, section)
	}
	for i, svc := range spec.Services {
		if name := ExtensionOf(svc); name != "" && !names[name] {
			v.fail(fmt.Sprintf("spec.services[%d].binary", i), "runs from the extension %q, which spec.extensions does not list", name)
		}
	}
}
