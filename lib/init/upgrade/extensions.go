package upgrade

import (
	"fmt"
	"slices"
	"strings"
)

// ErrExtensions is a bundle whose version the Node's trial boot would refuse for want of a system
// extension image built for it (ADR 0055).
var ErrExtensions = fmt.Errorf("the bundle's version has no extension image")

// Entry is an entry of spec.extensions, and Image a stored extension image with the naru version
// its manifest says it was built for.
type (
	Entry struct{ Name, SHA256 string }
	Image struct{ Name, SHA256, Version string }
)

// An entry with no SHA256 is satisfied by any stored image of its name built for version (ADR 0084).
//
// MissingExtensions returns the names of the extensions entries lists for which no entry names a
// stored image built for version: a Node booting version refuses the boot for each (ADR 0055).
func MissingExtensions(entries []Entry, stored []Image, version string) []string {
	have := map[string]bool{}
	for _, e := range entries {
		for _, img := range stored {
			if (img.SHA256 == e.SHA256 || e.SHA256 == "") && img.Name == e.Name && img.Version == version {
				have[e.Name] = true
			}
		}
	}
	var missing []string
	for _, e := range entries {
		if !have[e.Name] && !slices.Contains(missing, e.Name) {
			missing = append(missing, e.Name)
		}
	}
	return missing
}

// checkExtensions refuses a bundle of version when the Node's configuration lists an extension of
// which it stores no image built for version. A Node with no Extensions source checks nothing.
func (u *Upgrader) checkExtensions(version string) error {
	if u.cfg.Extensions == nil {
		return nil
	}
	entries, stored, err := u.cfg.Extensions()
	if err != nil {
		return fmt.Errorf("read the extensions to check the bundle against: %w", err)
	}
	missing := MissingExtensions(entries, stored, version)
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%w: the Node would refuse its boot on %s, since no stored image of %s is built for it and named in spec.extensions. "+
		"A pinned entry needs its image delivered with `naructl extension add` and its entry added beside the old one with `naructl apply --mode staged`; "+
		"an entry with no sha256 is satisfied by the image a bundle of the release carries (ADR 0084)",
		ErrExtensions, version, strings.Join(missing, ", "))
}
