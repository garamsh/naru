// Package guest reads guest.json, which image/guest/build.sh writes and the image carries: the
// guest kernels and initrds a Node boots Machines from, each with the path it has inside the image
// and its SHA-256 (ADR 0010). The tests' mvmd configuration is made from it, so a path or a digest
// is written once, by the build that produced the file.
package guest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
)

// File is one artifact: Path is where it is inside the image, SHA256 the digest of its bytes.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Manifest is guest.json. Kernels maps a kernel's name to its file in the form of the `kernels`
// map of mvmd's driver configuration (garamsh/mvmd#364), and DefaultKernel names the one a Machine
// boots on unless it chooses another.
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

// Read reads and checks the manifest at path.
func Read(path string) (Manifest, error) {
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

// Default is the default kernel's file.
func (m Manifest) Default() File { return m.Kernels[m.DefaultKernel] }
