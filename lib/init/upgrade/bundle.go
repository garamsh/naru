package upgrade

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"syscall"
)

// bundleExtensionDir is the directory of the extension members of a bundle, which follow the
// manifest (ADR 0084).
const bundleExtensionDir = "extensions/"

// The members of the upgrade bundle, in this order (ADR 0020).
const (
	bundleKernel   = "kernel.efi"
	bundleManifest = "manifest.json"
	// maxManifest bounds the manifest, which is a few hundred bytes.
	maxManifest = 64 << 10
)

// The ways an upgrade is refused. ErrBundle is the caller's: the digest does not match, or the
// body is not a bundle. ErrTooLarge is a bundle over the size cap or one the ESP cannot hold. ErrBusy
// is an upgrade that cannot start now.
var (
	ErrBundle   = errors.New("the bundle is refused")
	ErrTooLarge = errors.New("the bundle is too large")
	ErrBusy     = errors.New("an upgrade cannot start now")
)

// Manifest is manifest.json of a bundle.
type Manifest struct {
	Version string `json:"version"`
	Kernel  struct {
		Name   string `json:"name"`
		Bytes  int64  `json:"bytes"`
		SHA256 string `json:"sha256"`
	} `json:"kernel"`
	// Extensions are the members that follow manifest.json, in order.
	Extensions []BundleExtension `json:"extensions,omitempty"`
}

// BundleExtension describes the member extensions/<Name>.sqfs of a bundle.
type BundleExtension struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// stager takes the extension members of a bundle. wanted says whether the Node's configuration
// names the extension; stage stores a member the Node wants, whose digest the body must have, and
// reports whether the image is new to the store.
type stager struct {
	wanted func(name string) bool
	stage  func(name, sha256, version string, body io.Reader) (added bool, err error)
	// added collects the digests this bundle added, for Install to remove again on a refusal.
	added []string
}

// receive reads the bundle from body, which it caps at max bytes, and writes the kernel member to
// kernel as it arrives. It returns the manifest once the SHA-256 of the whole body is wantSHA and
// the manifest names the kernel it was given, by name, size and digest.
func receive(body io.Reader, max int64, wantSHA string, kernel io.Writer) (Manifest, error) {
	return receiveWith(body, max, wantSHA, kernel, nil)
}

// receiveWith is receive for a bundle whose extension members are given to st. A nil st discards
// them.
func receiveWith(body io.Reader, max int64, wantSHA string, kernel io.Writer, st *stager) (Manifest, error) {
	whole := sha256.New()
	src := io.TeeReader(&capped{r: body, left: max}, whole)
	tr := tar.NewReader(src)

	h, err := tr.Next()
	if err != nil {
		return Manifest{}, readError("the first member", err)
	}
	if h.Name != bundleKernel || h.Typeflag != tar.TypeReg {
		return Manifest{}, fmt.Errorf("%w: the first member is %q, not the regular file %s", ErrBundle, h.Name, bundleKernel)
	}
	sum := sha256.New()
	dst := &sink{w: io.MultiWriter(kernel, sum)}
	n, err := io.Copy(dst, tr)
	switch {
	case dst.err != nil:
		return Manifest{}, dst.err
	case err != nil:
		return Manifest{}, readError(bundleKernel, err)
	}

	h, err = tr.Next()
	if err != nil {
		return Manifest{}, readError("the second member", err)
	}
	if h.Name != bundleManifest || h.Typeflag != tar.TypeReg || h.Size > maxManifest {
		return Manifest{}, fmt.Errorf("%w: the second member is %q, not the regular file %s", ErrBundle, h.Name, bundleManifest)
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		return Manifest{}, readError(bundleManifest, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %s is not JSON: %v", ErrBundle, bundleManifest, err)
	}
	if err := receiveExtensions(tr, m, st); err != nil {
		return Manifest{}, err
	}
	// What follows the end blocks of the tar is part of what the digest covers.
	if _, err := io.Copy(io.Discard, src); err != nil {
		return Manifest{}, readError("the end of the bundle", err)
	}

	if got := hex.EncodeToString(whole.Sum(nil)); got != strings.ToLower(wantSHA) {
		return Manifest{}, fmt.Errorf("%w: the SHA-256 of the body is %s, not the %s asked for", ErrBundle, got, wantSHA)
	}
	if m.Version == "" {
		return Manifest{}, fmt.Errorf("%w: %s names no version", ErrBundle, bundleManifest)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); m.Kernel.Name != bundleKernel || m.Kernel.Bytes != n || n == 0 || m.Kernel.SHA256 != got {
		return Manifest{}, fmt.Errorf("%w: %s does not describe the kernel (it is %d bytes with SHA-256 %s)", ErrBundle, bundleManifest, n, got)
	}
	return m, nil
}

// receiveExtensions reads the members after the manifest, each of which the manifest lists, in
// order, with its size and digest. The end of the tar follows the last. A manifest that lists a
// name twice is refused before any member is read.
func receiveExtensions(tr *tar.Reader, m Manifest, st *stager) error {
	for i, e := range m.Extensions {
		if slices.ContainsFunc(m.Extensions[:i], func(p BundleExtension) bool { return p.Name == e.Name }) {
			return fmt.Errorf("%w: %s lists the extension %s twice", ErrBundle, bundleManifest, e.Name)
		}
	}
	for _, e := range m.Extensions {
		h, err := tr.Next()
		if err != nil {
			return readError("the extension "+e.Name, err)
		}
		if h.Name != bundleExtensionDir+e.Name+".sqfs" || h.Typeflag != tar.TypeReg || h.Size != e.Bytes {
			return fmt.Errorf("%w: the member %q is not the regular file %s%s.sqfs of %d bytes that %s lists", ErrBundle, h.Name, bundleExtensionDir, e.Name, e.Bytes, bundleManifest)
		}
		if st == nil || !st.wanted(e.Name) {
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return readError(h.Name, err)
			}
			continue
		}
		added, err := st.stage(e.Name, strings.ToLower(e.SHA256), m.Version, tr)
		if added {
			st.added = append(st.added, strings.ToLower(e.SHA256))
		}
		if err != nil {
			return err
		}
	}
	switch _, err := tr.Next(); {
	case err == nil:
		return fmt.Errorf("%w: a member follows the extensions %s lists", ErrBundle, bundleManifest)
	case !errors.Is(err, io.EOF):
		return readError("the end of the bundle", err)
	}
	return nil
}

// readError says what a failed read of the body is: the cap, or a body that is not a tar.
func readError(what string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrTooLarge), errors.Is(err, ErrBundle):
		return err
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, tar.ErrHeader):
		return fmt.Errorf("%w: %s: %v", ErrBundle, what, err)
	default:
		return fmt.Errorf("read the bundle (%s): %w", what, err)
	}
}

// capped fails with ErrTooLarge once more than left bytes are read.
type capped struct {
	r    io.Reader
	left int64
}

func (c *capped) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, ErrTooLarge
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n, ErrTooLarge
	}
	return n, err
}

// sink remembers the error of the writer behind it, so that a failed write to the ESP is told
// from a body that cannot be read. A file system with no room is ErrTooLarge.
type sink struct {
	w   io.Writer
	err error
}

func (s *sink) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err != nil {
		s.err = fmt.Errorf("write the trial slot: %w", err)
		if errors.Is(err, syscall.ENOSPC) {
			s.err = fmt.Errorf("%w: the ESP has no room for it: %v", ErrTooLarge, err)
		}
	}
	return n, err
}
