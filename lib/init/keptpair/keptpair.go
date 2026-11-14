// Package keptpair sets aside a certificate pair a service kept whose certificate lacks the
// Organization the service now needs.
package keptpair

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/garamsh/naru/lib/init/config"
)

// Suffix is appended to the name of a pair that is set aside.
const Suffix = ".o-less"

// SetAside renames the pair file of p to the same name with Suffix when its first certificate's
// subject has no Organization equal to p.Organization, and reports whether it did. A file that is
// missing, or whose first block is not a certificate that parses, is left as it is: the service
// reads it as it reads any pair it cannot use. A pair set aside by an earlier boot is replaced,
// so one is kept.
func SetAside(p config.KeptPair) (bool, error) {
	data, err := os.ReadFile(p.File)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", p.File, err)
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return false, nil
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || slices.Contains(cert.Subject.Organization, p.Organization) {
		return false, nil
	}
	if err := os.Rename(p.File, p.File+Suffix); err != nil {
		return false, fmt.Errorf("set %s aside: %w", p.File, err)
	}
	return true, nil
}
