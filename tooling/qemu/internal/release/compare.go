package release

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ByDesignPattern matches the assets that differ in every build on purpose: the data disk and the
// test user-data carry a fresh registry authority, a fresh gaon authority and fresh tokens. It is
// the same expression as the skip pattern of image/repro.sh, which cannot import it; a test fails
// when the two differ.
const ByDesignPattern = `^(naru-data-.*\.qcow2|naru-node-[0-9]+\.test\.yaml)$`

var byDesign = regexp.MustCompile(ByDesignPattern)

// parseSums reads a SHA256SUMS file into name to digest.
func parseSums(what string, b []byte) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || sum == "" || name == "" {
			return nil, fmt.Errorf("%s: %q is not a line of SHA256SUMS", what, line)
		}
		out[name] = sum
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s lists no asset", what)
	}
	return out, nil
}

// Compare checks the SHA256SUMS of a published release against the one a build of the same tag
// wrote. It returns nil when every asset agrees, and otherwise an error that names each asset that
// differs or that only one side lists. The by-design assets (ByDesignPattern) are left out.
func Compare(published, built []byte) error {
	a, err := parseSums("the published SHA256SUMS", published)
	if err != nil {
		return err
	}
	b, err := parseSums("the new SHA256SUMS", built)
	if err != nil {
		return err
	}
	var bad []string
	for name, sum := range a {
		if byDesign.MatchString(name) {
			continue
		}
		other, ok := b[name]
		switch {
		case !ok:
			bad = append(bad, name+": only the published release has it")
		case other != sum:
			bad = append(bad, fmt.Sprintf("%s: published %s, new build %s", name, sum, other))
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok && !byDesign.MatchString(name) {
			bad = append(bad, name+": only the new build has it")
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("the tag's release exists and differs from this build in %d asset(s):\n  %s", len(bad), strings.Join(bad, "\n  "))
}
