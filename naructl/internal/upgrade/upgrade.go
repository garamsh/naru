// Package upgrade is naructl's side of the in-place upgrade (ADR 0025): it finds a bundle's digest
// in a SHA256SUMS file and checks the bundle against it. The Node checks the digest again; this
// check refuses a bundle that was damaged on the way to the operator before any byte is sent. The
// bundle itself goes to the Node through client.Client.Upgrade.
package upgrade

import (
	"archive/tar"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// SumFor returns the SHA-256 that sums, the text of a SHA256SUMS file, lists for the file called
// name. A line is a digest, a space, and a file name that may have a "*" before it, as sha256sum
// writes it; a name with a directory is compared by its last element. A file that is not listed,
// or is listed with two digests, is an error.
func SumFor(sums io.Reader, name string) (string, error) {
	found := ""
	sc := bufio.NewScanner(sums)
	for sc.Scan() {
		sum, file, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		file = strings.TrimPrefix(strings.TrimSpace(file), "*")
		if !ok || filepath.Base(file) != name {
			continue
		}
		if len(sum) != sha256.Size*2 || !isHex(sum) {
			return "", fmt.Errorf("the line for %s does not start with a SHA-256", name)
		}
		sum = strings.ToLower(sum)
		if found != "" && found != sum {
			return "", fmt.Errorf("the sums file lists %s with two different digests", name)
		}
		found = sum
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("the sums file has no line for %s", name)
	}
	return found, nil
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// Check returns nil when the file at path has the SHA-256 want.
func Check(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s has the SHA-256 %s, and the sums file says %s", path, got, want)
	}
	return nil
}

// BundleVersion returns the version the manifest of the bundle at path names. A bundle holds the
// kernel and then manifest.json (ADR 0020); the Node checks the whole of it, and this only reads
// the version out.
func BundleVersion(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err != nil {
			return "", fmt.Errorf("%s holds no manifest.json: %w", path, err)
		}
		if h.Name != "manifest.json" {
			continue
		}
		var m struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(io.LimitReader(tr, maxManifest)).Decode(&m); err != nil {
			return "", fmt.Errorf("%s: manifest.json: %w", path, err)
		}
		if m.Version == "" {
			return "", fmt.Errorf("%s: manifest.json names no version", path)
		}
		return m.Version, nil
	}
}

// maxManifest bounds the manifest, which is a few hundred bytes.
const maxManifest = 64 << 10

// Entry is a spec.extensions entry, and Image a stored extension image with the naru version its
// manifest says it was built for.
type (
	Entry struct{ Name, SHA256 string }
	Image struct{ Name, SHA256, Version string }
)

// MissingExtensions returns the names of the extensions entries lists for which no entry names a
// stored image built for version: a Node booting version refuses the boot for each (ADR 0055).
func MissingExtensions(entries []Entry, stored []Image, version string) []string {
	have := map[string]bool{}
	for _, e := range entries {
		for _, img := range stored {
			if img.SHA256 == e.SHA256 && img.Name == e.Name && img.Version == version {
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
