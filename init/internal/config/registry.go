package config

import (
	"fmt"
	"net"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Registry is how the Node reaches one container registry: the authority it trusts and the
// credential it presents (docs/architecture/adr/0026-registry-credentials-and-trust-are-configuration-and-an-ecr-credential-is-renewed-through-iam-roles-anywhere.md).
type Registry struct {
	// Host is the registry's name as an image reference spells it, with a port after a colon
	// when it is not the default.
	Host string `yaml:"host"`
	// CA is the PEM of the authorities the Node trusts for this registry, in addition to the
	// roots the image carries. It is empty when the registry's certificate chains to those.
	CA string `yaml:"ca,omitempty"`
	// Credential is what the Node presents. It is nil for a registry that asks for none.
	Credential *Credential `yaml:"credential,omitempty"`
}

// Credential is a username and a password, or the name of a docker credential helper, never both.
type Credential struct {
	Username string `yaml:"username,omitempty"`
	// Password is private material: GET /v1/config answers REDACTED in its place.
	Password string `yaml:"password,omitempty"`
	// Helper names the program docker-credential-<helper> that mvmd runs for the registry.
	Helper string `yaml:"helper,omitempty"`
}

// AWS is the identity the credential helpers of the Node use: an IAM Roles Anywhere profile, which
// trades the Node's certificate for the temporary credentials of a role.
type AWS struct {
	Region         string `yaml:"region"`
	TrustAnchorArn string `yaml:"trustAnchorArn"`
	ProfileArn     string `yaml:"profileArn"`
	RoleArn        string `yaml:"roleArn"`
	// Certificate and Key are paths on the Node of the certificate and its private key; one
	// file may hold both.
	Certificate string `yaml:"certificate"`
	Key         string `yaml:"key"`
}

// Registry returns the entry for host, and whether there is one.
func (s Spec) Registry(host string) (Registry, bool) {
	for _, r := range s.Registries {
		if r.Host == host {
			return r, true
		}
	}
	return Registry{}, false
}

var helperRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func (v *validation) registries(registries []Registry) {
	seen := map[string]bool{}
	for i, r := range registries {
		field := fmt.Sprintf("spec.registries[%d]", i)
		switch {
		case !validRegistryHost(r.Host):
			v.fail(field+".host", "must be a host name or an IP address with an optional port, got %q", r.Host)
		case seen[r.Host]:
			v.fail(field+".host", "%q is listed twice", r.Host)
		}
		seen[r.Host] = true
		if r.CA != "" {
			if _, err := parseCertificate(r.CA); err != nil {
				v.fail(field+".ca", "%v", err)
			}
		}
		c := r.Credential
		if c == nil {
			continue
		}
		switch {
		case c.Helper != "" && (c.Username != "" || c.Password != ""):
			v.fail(field+".credential", "gives both a helper and a username or password; give one")
		case c.Helper != "":
			if !helperRE.MatchString(c.Helper) {
				v.fail(field+".credential.helper", "must be a program name of letters, digits, '-', '_' and '.', got %q", c.Helper)
			}
		case c.Username == "" || c.Password == "":
			v.fail(field+".credential", "must give a helper, or a username and a password")
		}
	}
}

func (v *validation) aws(a *AWS) {
	if a == nil {
		return
	}
	for _, f := range []struct{ name, value, prefix string }{
		{"region", a.Region, ""},
		{"trustAnchorArn", a.TrustAnchorArn, "arn:"},
		{"profileArn", a.ProfileArn, "arn:"},
		{"roleArn", a.RoleArn, "arn:"},
	} {
		switch {
		case f.value == "":
			v.fail("spec.aws."+f.name, "must be given")
		case strings.ContainsAny(f.value, " \t\r\n\"'\\"):
			v.fail("spec.aws."+f.name, "must not hold white space, quotes or a backslash, got %q", f.value)
		case !strings.HasPrefix(f.value, f.prefix):
			v.fail("spec.aws."+f.name, "must be an ARN, got %q", f.value)
		}
	}
	for _, f := range []struct{ name, value string }{{"certificate", a.Certificate}, {"key", a.Key}} {
		if !isCleanAbs(f.value) || strings.ContainsAny(f.value, " \t\r\n\"'\\") {
			v.fail("spec.aws."+f.name, "must be a clean absolute path without white space or quotes, got %q", f.value)
		}
	}
}

// validRegistryHost reports whether h is a host name or an IP address, with an optional port.
func validRegistryHost(h string) bool {
	host := h
	if hh, p, err := net.SplitHostPort(h); err == nil {
		if n, err := strconv.ParseUint(p, 10, 16); err != nil || n == 0 {
			return false
		}
		host = hh
	}
	if host == "" || path.Base(host) != host {
		return false
	}
	return net.ParseIP(host) != nil || (len(host) <= 253 && hostRE.MatchString(host))
}
