package groups

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// maxBootSetName is the longest name mvmd takes for a boot set.
const maxBootSetName = 63

const bootSetNamePrefix = "naru-node-"

var notBootSetName = regexp.MustCompile(`[^a-z0-9-]`)

// BootSetName is the name a release's boot set is offered under: naru-node- and the version in
// lower case with every character outside a-z, 0-9 and the hyphen replaced by a hyphen, so
// v0.4.0 is naru-node-v0-4-0. It fails for a version whose name is longer than mvmd takes, and
// for one that already starts with naru-node-: that is a boot set name, which no release makes a
// name of.
func BootSetName(version string) (string, error) {
	if strings.HasPrefix(strings.ToLower(version), bootSetNamePrefix) {
		return "", fmt.Errorf("version %q is a boot set name, not a release version", version)
	}
	name := bootSetNamePrefix + notBootSetName.ReplaceAllString(strings.ToLower(version), "-")
	if len(name) > maxBootSetName {
		return "", fmt.Errorf("version %q makes the boot set name %q, longer than the %d characters mvmd takes", version, name, maxBootSetName)
	}
	return name, nil
}

// ResolveBootSet maps a template's nodeVersion to the boot set a host that offers the names
// offered boots it from. A nodeVersion that is a name the host offers is that name; otherwise it
// is a release version, and its boot set is BootSetName's. offers reports whether the host has the
// set; name is empty when the version names none.
func ResolveBootSet(nodeVersion string, offered []string) (name string, offers bool) {
	if slices.Contains(offered, nodeVersion) {
		return nodeVersion, true
	}
	name, err := BootSetName(nodeVersion)
	if err != nil {
		return "", false
	}
	return name, slices.Contains(offered, name)
}

// bootSetOn is the boot set the template boots on h, or empty for a template that names none.
func bootSetOn(t Template, h Host) string {
	if t.Form != FormOS || t.NodeVersion == "" {
		return ""
	}
	name, _ := ResolveBootSet(t.NodeVersion, h.NodeVersions)
	return name
}
