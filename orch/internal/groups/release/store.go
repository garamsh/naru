// Package release is the store of the release assets of the Node versions, and the check of a
// release against its signature before any byte of it leaves orch.
package release

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/garamsh/naru/orch/internal/groups"
)

// The reasons a release does not verify. Each wraps the detail and names one check.
var (
	ErrNotFound  = errors.New("release asset not found")
	ErrStore     = errors.New("release store unreadable")
	ErrSignature = errors.New("signature check failed")
	ErrIdentity  = errors.New("signer identity check failed")
	ErrChecksum  = errors.New("checksum check failed")
	ErrManifest  = errors.New("manifest check failed")
)

const (
	maxSmall = 1 << 20  // SHA256SUMS and the bundle
	maxMani  = 64 << 10 // the manifest
	// maxBootSet is what the host accepts of one upload, less room for the form around it.
	maxBootSet = 1<<30 - 1<<20
)

var versionSyntax = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// DefaultIssuer is the OIDC issuer of the GitHub Actions workflows that sign the releases.
const DefaultIssuer = "https://token.actions.githubusercontent.com"

// Config is where the releases are and who signs them.
type Config struct {
	// Location is an https URL or a directory.
	Location string
	// TrustRoot is a trusted_root.json; empty means the one embedded.
	TrustRoot string
	// Issuer and Identity are the OIDC issuer and the certificate identity of the signer. An
	// Identity ending in "*" is a prefix.
	Issuer, Identity string
	// Client reads an https store; nil means http.DefaultClient.
	Client *http.Client
}

// Store is a groups.ReleaseStore that verifies every release it returns.
type Store struct {
	src source
	sig signature
}

var _ groups.ReleaseStore = (*Store)(nil)

// New returns the store of c, or the reason c cannot be one.
func New(c Config) (*Store, error) {
	sig, err := newSignature(c.TrustRoot, c.Issuer, c.Identity)
	if err != nil {
		return nil, err
	}
	src, err := newSource(c.Location, c.Client)
	if err != nil {
		return nil, err
	}
	return &Store{src: src, sig: sig}, nil
}

type manifest struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Kernel    file   `json:"kernel"`
	Initramfs file   `json:"initramfs"`
}

type file struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Fetch verifies the release of version and returns its boot set. The signature over SHA256SUMS
// is checked first, then the manifest and the names and digests of the files against it. The
// kernel and the initramfs are checked as they are read.
func (s *Store) Fetch(ctx context.Context, version string) (groups.BootSet, error) {
	name, err := groups.BootSetName(version)
	if err != nil || !versionSyntax.MatchString(version) || strings.Contains(version, "..") {
		return groups.BootSet{}, fmt.Errorf("%w: %q names no release", ErrManifest, version)
	}
	raw, err := readAll(ctx, s.src, "SHA256SUMS.sigstore.json", maxSmall)
	if err != nil {
		return groups.BootSet{}, err
	}
	sums, err := readAll(ctx, s.src, "SHA256SUMS", maxSmall)
	if err != nil {
		return groups.BootSet{}, err
	}
	if err := s.sig.check(raw, sums); err != nil {
		return groups.BootSet{}, err
	}
	digests, err := parseSums(sums)
	if err != nil {
		return groups.BootSet{}, err
	}
	mname := "naru-node-" + version + ".json"
	mraw, err := readAll(ctx, s.src, mname, maxMani)
	if err != nil {
		return groups.BootSet{}, err
	}
	if got := sha256.Sum256(mraw); digests[mname] != hex.EncodeToString(got[:]) {
		return groups.BootSet{}, fmt.Errorf("%w: %s is not the file SHA256SUMS lists", ErrChecksum, mname)
	}
	var m manifest
	if err := json.Unmarshal(mraw, &m); err != nil {
		return groups.BootSet{}, fmt.Errorf("%w: %s: %v", ErrManifest, mname, err)
	}
	if m.Name != name || m.Version != version {
		return groups.BootSet{}, fmt.Errorf("%w: %s is for %q at %q, not %q at %q", ErrManifest, mname, m.Name, m.Version, name, version)
	}
	for _, f := range []file{m.Kernel, m.Initramfs} {
		if f.File == "" || f.File != filepath.Base(f.File) || f.File == "." || f.Bytes <= 0 {
			return groups.BootSet{}, fmt.Errorf("%w: %s names a file that is not a file name beside it", ErrManifest, mname)
		}
		if want, ok := digests[f.File]; !ok || want != strings.ToLower(f.SHA256) {
			return groups.BootSet{}, fmt.Errorf("%w: %s is not listed in SHA256SUMS with the digest the manifest gives", ErrChecksum, f.File)
		}
	}
	if m.Kernel.Bytes+m.Initramfs.Bytes > maxBootSet {
		return groups.BootSet{}, fmt.Errorf("%w: the files are larger than the host accepts", ErrManifest)
	}
	return groups.BootSet{
		Name:            name,
		KernelSHA256:    digests[m.Kernel.File],
		InitramfsSHA256: digests[m.Initramfs.File],
		Open: func(ctx context.Context) (io.ReadCloser, io.ReadCloser, error) {
			k, err := s.open(ctx, m.Kernel)
			if err != nil {
				return nil, nil, err
			}
			i, err := s.open(ctx, m.Initramfs)
			if err != nil {
				k.Close()
				return nil, nil, err
			}
			return k, i, nil
		},
	}, nil
}

func (s *Store) open(ctx context.Context, f file) (io.ReadCloser, error) {
	rc, err := s.src.open(ctx, f.File)
	if err != nil {
		return nil, err
	}
	return &checked{rc: rc, h: sha256.New(), want: strings.ToLower(f.SHA256), size: f.Bytes, name: f.File}, nil
}

// parseSums reads a SHA256SUMS into digests by file name.
func parseSums(b []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		d, n, ok := strings.Cut(line, " ")
		n = strings.TrimPrefix(strings.TrimLeft(n, " "), "*")
		if raw, err := hex.DecodeString(d); !ok || err != nil || len(raw) != sha256.Size || n == "" {
			return nil, fmt.Errorf("%w: SHA256SUMS has a line that is not a digest and a name", ErrChecksum)
		}
		out[n] = strings.ToLower(d)
	}
	return out, sc.Err()
}

// checked is a reader that ends in an error, and not in io.EOF, unless what it read is the
// size and the digest the release names.
type checked struct {
	rc   io.ReadCloser
	h    hash.Hash
	want string
	size int64
	n    int64
	name string
}

func (c *checked) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.h.Write(p[:n])
	c.n += int64(n)
	if c.n > c.size {
		return n, fmt.Errorf("%w: %s is larger than the manifest says", ErrChecksum, c.name)
	}
	if errors.Is(err, io.EOF) {
		if c.n != c.size || hex.EncodeToString(c.h.Sum(nil)) != c.want {
			return n, fmt.Errorf("%w: %s is not the file SHA256SUMS lists", ErrChecksum, c.name)
		}
	}
	return n, err
}

func (c *checked) Close() error { return c.rc.Close() }
