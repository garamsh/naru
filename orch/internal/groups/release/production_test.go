package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dev13 holds three assets of the published release v0.1.0-dev.13 of garamsh/naru, as
// `gh release download` fetched them: SHA256SUMS, SHA256SUMS.sigstore.json and the manifest.
// The kernel and the initramfs are read lazily and are not needed to verify the release.
const (
	dev13Dir     = "testdata/dev13"
	dev13Version = "v0.1.0-dev.13"
)

// copyDev13 copies the three assets into a new directory and returns it.
func copyDev13(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(dev13Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("%s holds %d files, want the three assets", dev13Dir, len(entries))
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dev13Dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestProductionVerifierOnTheDev13Release runs Fetch with New: the embedded trust root and the
// verifier's production requirements (a signed certificate timestamp, a transparency log entry,
// an observer timestamp and an inclusion proof) against the bundle the release workflow made.
// Each refusal is read beside the same release accepted.
func TestProductionVerifierOnTheDev13Release(t *testing.T) {
	good := Config{Location: copyDev13(t), Issuer: DefaultIssuer, Identity: DefaultIdentity}
	set, err := mustNew(t, good).Fetch(context.Background(), dev13Version)
	if err != nil {
		t.Fatalf("the published release does not verify: %v", err)
	}
	if set.Name != "naru-node-v0-1-0-dev-13" ||
		set.KernelSHA256 != "f21f737991f56abf47b27326e35456b58901ce4963da974180d6aa00d1974832" ||
		set.InitramfsSHA256 != "a3c6306b6e19244454bdefac479528ea88ca11f4cce03eac397b65f4bcad018d" {
		t.Fatalf("boot set = %+v", set)
	}

	for _, c := range []struct {
		name   string
		config func(c *Config)
		tamper func(t *testing.T, dir string)
		want   error
	}{
		{name: "one byte of SHA256SUMS flipped", want: ErrSignature, tamper: func(t *testing.T, dir string) {
			b := mustRead(t, dir, "SHA256SUMS")
			if b[0] == '0' {
				b[0] = '1'
			} else {
				b[0] = '0'
			}
			writeFile(t, dir, "SHA256SUMS", b)
		}},
		{name: "another tag of this repository", want: ErrIdentity, config: func(c *Config) {
			c.Identity = "https://github.com/garamsh/naru/.github/workflows/release.yml@refs/tags/v0.1.0-dev.12"
		}},
		{name: "another repository's prefix", want: ErrIdentity, config: func(c *Config) {
			c.Identity = "https://github.com/garamsh/naru-evil/*"
		}},
		{name: "another issuer", want: ErrIdentity, config: func(c *Config) { c.Issuer = "https://accounts.google.com" }},
		{name: "inclusion proof removed", want: ErrSignature, tamper: func(t *testing.T, dir string) {
			var b map[string]any
			if err := json.Unmarshal(mustRead(t, dir, "SHA256SUMS.sigstore.json"), &b); err != nil {
				t.Fatal(err)
			}
			entries := b["verificationMaterial"].(map[string]any)["tlogEntries"].([]any)
			if _, ok := entries[0].(map[string]any)["inclusionProof"]; !ok {
				t.Fatal("the bundle has no inclusion proof to remove: the control would degenerate")
			}
			delete(entries[0].(map[string]any), "inclusionProof")
			out, _ := json.Marshal(b)
			writeFile(t, dir, "SHA256SUMS.sigstore.json", out)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{Location: copyDev13(t), Issuer: DefaultIssuer, Identity: DefaultIdentity}
			if c.config != nil {
				c.config(&cfg)
			}
			if c.tamper != nil {
				c.tamper(t, cfg.Location)
			}
			_, err := mustNew(t, cfg).Fetch(context.Background(), dev13Version)
			if !errors.Is(err, c.want) {
				t.Fatalf("Fetch = %v, want %v", err, c.want)
			}
			if c.name == "inclusion proof removed" && !strings.Contains(err.Error(), "inclusion proof") {
				t.Fatalf("Fetch = %v, want the refusal to name the inclusion proof", err)
			}
		})
	}
}

func mustNew(t *testing.T, c Config) *Store {
	t.Helper()
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func writeFile(t *testing.T, dir, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The built-in trust root is the file docs/architecture/orch.md names by digest; a change to one
// without the other fails here.
func TestEmbeddedTrustRootIsTheOneTheArchitectureDocumentNames(t *testing.T) {
	const documented = "844a1c6de3986c9f02070266b25e0d1a2fa99ceccc89f6b9ad90aae47b62a16e"
	sum := sha256.Sum256(trustedRootJSON)
	if got := hex.EncodeToString(sum[:]); got != documented {
		t.Fatalf("trusted_root.json is %s, orch.md documents %s", got, documented)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "architecture", "orch.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), documented) {
		t.Fatalf("orch.md does not name %s", documented)
	}
}
