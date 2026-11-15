// Package seed writes the install seed: the NoCloud drive a host boots the installer with to join
// orch (docs/architecture/orch.md, ADR 0059). Its user-data is the join form and nothing else: where
// orch is, the digest of the authority to trust there, and the enrollment key. No network-config
// goes with it, so the installer asks for an address by DHCP.
package seed

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/iso9660"
)

// Label is the volume label NoCloud looks for.
const Label = "cidata"

var (
	digest = regexp.MustCompile(`^[0-9a-f]{64}$`)
	label  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// key is the shape of the secret orch makes, which init's join form refuses anything else than.
	key = regexp.MustCompile(`^naru-ek-[0-9a-f]{64}$`)
)

// Join is what a host needs to enroll: orch's URL, the SHA-256 of the authority orch's certificate
// chains to, the enrollment key, and optionally the name the host is listed under until it is
// admitted.
type Join struct {
	OrchURL   string
	CASHA256  string
	EnrollKey string
	Name      string
}

// Form is the join form as the user-data carries it.
type Form struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   *meta    `yaml:"metadata,omitempty"`
	Spec       formSpec `yaml:"spec"`
}

type meta struct {
	Name string `yaml:"name"`
}

type formSpec struct {
	Join joinSpec `yaml:"join"`
}

type joinSpec struct {
	Orch      orchSpec `yaml:"orch"`
	EnrollKey string   `yaml:"enrollKey"`
}

type orchSpec struct {
	URL      string `yaml:"url"`
	CASHA256 string `yaml:"caSHA256"`
}

// Check reports the first thing wrong with j.
func (j Join) Check() error {
	if err := j.CheckTarget(); err != nil {
		return err
	}
	if !key.MatchString(j.EnrollKey) {
		return errors.New("the enrollment key is not naru-ek- and 64 lower-case hex digits, which is what enroll-key create prints")
	}
	return nil
}

// CheckTarget reports the first thing wrong with j other than its key, so that a caller who is
// about to make the key can refuse a seed before it does.
func (j Join) CheckTarget() error {
	u, err := url.Parse(j.OrchURL)
	switch {
	case err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.User != nil:
		return fmt.Errorf("the orch URL %q is not an https URL of a host and a port", j.OrchURL)
	case !digest.MatchString(j.CASHA256):
		return errors.New("the orch authority's SHA-256 is not 64 lower-case hex digits")
	case j.Name != "" && !label.MatchString(j.Name):
		return fmt.Errorf("the name %q is not a DNS label", j.Name)
	}
	return nil
}

// UserData is the join form.
func (j Join) UserData() ([]byte, error) {
	if err := j.Check(); err != nil {
		return nil, err
	}
	f := Form{APIVersion: "naru.garam.sh/v1alpha1", Kind: "NodeConfig", Spec: formSpec{Join: joinSpec{
		Orch: orchSpec{URL: j.OrchURL, CASHA256: j.CASHA256}, EnrollKey: j.EnrollKey,
	}}}
	if j.Name != "" {
		f.Metadata = &meta{Name: j.Name}
	}
	return yaml.Marshal(f)
}

// ISO is the seed as an ISO 9660 volume labelled cidata, holding user-data and a meta-data that
// names the instance.
func (j Join) ISO() ([]byte, error) {
	ud, err := j.UserData()
	if err != nil {
		return nil, err
	}
	return iso9660.Write(Label, []iso9660.File{
		{Name: "user-data", Data: ud},
		{Name: "meta-data", Data: []byte("instance-id: naru-install-seed\n")},
	})
}
