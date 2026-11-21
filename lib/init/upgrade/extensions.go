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

// candidates returns the distinct stored images of name, built for version, that init's boot would
// choose among: those a pinned entry names and, for an entry with no SHA256, every one of that
// name (ADR 0055, 0084). The boot mounts the one and refuses the boot of none or of several.
func candidates(entries []Entry, stored []Image, name, version string) []Image {
	var out []Image
	for _, e := range entries {
		if e.Name != name {
			continue
		}
		for _, img := range stored {
			if img.Name == name && img.Version == version && (e.SHA256 == "" || img.SHA256 == e.SHA256) && !slices.Contains(out, img) {
				out = append(out, img)
			}
		}
	}
	return out
}

// entryNames returns the distinct names of entries, in order.
func entryNames(entries []Entry) []string {
	var names []string
	for _, e := range entries {
		if !slices.Contains(names, e.Name) {
			names = append(names, e.Name)
		}
	}
	return names
}

// MissingExtensions returns the names of the extensions entries lists for which no stored image
// built for version is named: a Node booting version refuses the boot for each (ADR 0055).
func MissingExtensions(entries []Entry, stored []Image, version string) []string {
	return namesWith(entries, stored, version, func(n int) bool { return n == 0 })
}

// AmbiguousExtensions returns the names of the extensions entries lists for which more than one
// stored image built for version is named: a Node booting version refuses the boot for each, since
// it mounts only one (ADR 0055).
func AmbiguousExtensions(entries []Entry, stored []Image, version string) []string {
	return namesWith(entries, stored, version, func(n int) bool { return n > 1 })
}

func namesWith(entries []Entry, stored []Image, version string, count func(int) bool) []string {
	var out []string
	for _, name := range entryNames(entries) {
		if count(len(candidates(entries, stored, name, version))) {
			out = append(out, name)
		}
	}
	return out
}

// checkExtensions refuses a bundle of version when the Node's configuration lists an extension of
// which it stores no image, or more than one, built for version. A Node with no Extensions source checks nothing.
func (u *Upgrader) checkExtensions(version string) error {
	if u.cfg.Extensions == nil {
		return nil
	}
	entries, stored, err := u.cfg.Extensions()
	if err != nil {
		return fmt.Errorf("read the extensions to check the bundle against: %w", err)
	}
	missing := MissingExtensions(entries, stored, version)
	ambiguous := AmbiguousExtensions(entries, stored, version)
	if len(missing) == 0 && len(ambiguous) == 0 {
		return nil
	}
	if len(ambiguous) > 0 {
		return fmt.Errorf("%w: the Node would refuse its boot on %s, since more than one stored image of %s is built for it and named in spec.extensions, and it mounts one. "+
			"Remove the images or entries that are not wanted (ADR 0084)",
			ErrExtensions, version, strings.Join(ambiguous, ", "))
	}
	return fmt.Errorf("%w: the Node would refuse its boot on %s, since no stored image of %s is built for it and named in spec.extensions. "+
		"A pinned entry needs its image delivered with `naructl extension add` and its entry added beside the old one with `naructl apply --mode staged`; "+
		"an entry with no sha256 is satisfied by the image a bundle of the release carries (ADR 0084)",
		ErrExtensions, version, strings.Join(missing, ", "))
}
