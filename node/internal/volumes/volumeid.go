package volumes

import (
	"fmt"
	"regexp"
	"strings"
)

// nameRE is a volume's name, which a PersistentVolume's name is: a lowercase DNS name.
var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,200}$`)

// volumeID names a volume: its kind and its request name, as "local-<name>" or "block-<name>". A
// volumeID is made by newID or parseID only, so its text is a file name and names no other path.
type volumeID string

// newID returns the id of the volume of kind named name, or an error when name is not a volume name.
func newID(kind, name string) (volumeID, error) {
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("the volume name %q is not a lowercase DNS name", name)
	}
	return volumeID(kind + "-" + name), nil
}

// parseID reads the id a CSI request carries, which no one has vouched for.
func parseID(s string) (volumeID, error) {
	kind, name, ok := strings.Cut(s, "-")
	if !ok || (kind != kindLocal && kind != kindBlock) {
		return "", fmt.Errorf("the volume id %q is neither local-<name> nor block-<name>", s)
	}
	return newID(kind, name)
}

func (v volumeID) kind() string { return strings.SplitN(string(v), "-", 2)[0] }
