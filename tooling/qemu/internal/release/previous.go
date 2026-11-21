package release

import (
	"fmt"

	"golang.org/x/mod/semver"
)

// Previous returns the release with the highest version below tag among tags, by SemVer 2.0.0
// precedence, so v0.1.0-dev.9 is below v0.1.0-dev.10, which is below v0.1.0-rc.1, which is below
// v0.1.0. The tag itself is left out, and so is a name that is not a SemVer version. Neither
// publish time nor commit date plays a part, so a release published after the commit of a lower
// version was made is still found. It returns "" when no release is below tag.
func Previous(tag string, tags []string) (string, error) {
	if !semver.IsValid(tag) {
		return "", fmt.Errorf("%q is not a SemVer version (vMAJOR.MINOR.PATCH[-prerelease])", tag)
	}
	prev := ""
	for _, t := range tags {
		if !semver.IsValid(t) || semver.Compare(t, tag) >= 0 {
			continue
		}
		if prev == "" || semver.Compare(t, prev) > 0 {
			prev = t
		}
	}
	return prev, nil
}
