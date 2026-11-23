package release

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Component is one upstream package that a pin.env under image/ names: a source tarball (URL and
// SHA256) or a git commit (Repo and Commit), with the SPDX licence expression its <PREFIX>_LICENSE
// line states.
type Component struct {
	Name, Version, License string
	URL, SHA256            string
	Repo, Commit           string
	// CommitFrom is the module whose version in host/go.mod names the commit, for a component
	// whose commit is not written in its pin.env (mvmd's, which host's Go client is built from).
	CommitFrom string
	// Pin is the pin.env the component is in, relative to the root.
	Pin string
}

// CommitOf returns the commit that the version of module in root/host/go.mod names, which go
// resolves from the pseudo-version. It is a variable so that a test needs no network.
var CommitOf = func(root, module string) (string, error) {
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command("go", append([]string{"list", "-m"}, args...)...)
		cmd.Dir = filepath.Join(root, "host")
		cmd.Env = append(os.Environ(), "GOWORK=off")
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list -m %v in %s: %w", args, cmd.Dir, err)
		}
		return out, nil
	}
	version, err := run("-f", "{{.Version}}", module)
	if err != nil {
		return "", err
	}
	out, err := run("-json", module+"@"+strings.TrimSpace(string(version)))
	if err != nil {
		return "", err
	}
	var m struct{ Origin struct{ Hash string } }
	if err := json.Unmarshal(out, &m); err != nil || len(m.Origin.Hash) != 40 {
		return "", fmt.Errorf("go did not resolve %s@%s to a commit", module, strings.TrimSpace(string(version)))
	}
	return m.Origin.Hash, nil
}

var pinLine = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=(.*)$`)

// ReadComponents returns the components of every pin.env under root/image, sorted by name. A pin
// without a <PREFIX>_LICENSE line, a tarball without a SHA-256 and a commit pin without a commit
// are errors: the SBOM states each one, and a release that cannot would ship a package nobody can
// account for.
func ReadComponents(root string) ([]Component, error) {
	var out []Component
	seen := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, "image"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "pin.env" {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		cs, err := parsePin(filepath.ToSlash(rel), string(b))
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.Commit == "" && c.CommitFrom != "" {
				if c.Commit, err = CommitOf(root, c.CommitFrom); err != nil {
					return fmt.Errorf("%s: %w", rel, err)
				}
				if c.Version == "" {
					c.Version = c.Commit
				}
			}
			if prev, dup := seen[c.Name]; dup {
				return fmt.Errorf("%s: %s is also pinned in %s", rel, c.Name, prev)
			}
			seen[c.Name] = c.Pin
			out = append(out, c)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// parsePin reads the KEY=value lines of one pin.env (single or double quotes around a value are
// dropped) and returns a component for each prefix that has a _URL or a _REPO line.
func parsePin(rel, text string) ([]Component, error) {
	kv := map[string]string{}
	var prefixes []string
	for _, line := range strings.Split(text, "\n") {
		m := pinLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		v := m[2]
		if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		kv[m[1]] = v
		for _, suf := range []string{"_URL", "_REPO"} {
			if p, ok := strings.CutSuffix(m[1], suf); ok {
				prefixes = append(prefixes, p)
			}
		}
	}
	var out []Component
	for _, p := range prefixes {
		c := Component{
			Name: strings.ToLower(strings.ReplaceAll(p, "_", "-")), Pin: rel,
			URL: kv[p+"_URL"], SHA256: kv[p+"_SHA256"], Repo: kv[p+"_REPO"], Commit: kv[p+"_COMMIT"], CommitFrom: kv[p+"_COMMIT_FROM"], License: kv[p+"_LICENSE"],
			Version: kv[p+"_VERSION"],
		}
		if c.Version == "" {
			c.Version = kv[p+"_DATE"]
		}
		switch {
		case c.License == "":
			return nil, fmt.Errorf("%s: %s_LICENSE is missing: state the SPDX licence expression of %s", rel, p, c.Name)
		case c.Repo != "" && c.Commit == "" && c.CommitFrom == "":
			return nil, fmt.Errorf("%s: %s_REPO has no %s_COMMIT and no %s_COMMIT_FROM", rel, p, p, p)
		case c.URL != "" && c.SHA256 == "":
			return nil, fmt.Errorf("%s: %s_URL has no %s_SHA256", rel, p, p)
		}
		if c.Version == "" {
			c.Version = c.Commit
		}
		out = append(out, c)
	}
	return out, nil
}

var copyleftToken = regexp.MustCompile(`(^|[ (])(GPL|LGPL|AGPL)-`)

// Copyleft reports whether the component's licence expression names a GPL, LGPL or AGPL licence:
// the release then carries its exact source (ADR 0076).
func (c Component) Copyleft() bool { return copyleftToken.MatchString(c.License) }

// TarballName is the file name the component's URL ends in, which is how the build directories
// of make image name a download.
func (c Component) TarballName() string { return path.Base(c.URL) }
