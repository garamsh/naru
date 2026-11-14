package upgrade

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
)

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
}

// receive reads the bundle from body, which it caps at max bytes, and writes the kernel member to
// kernel as it arrives. It returns the manifest once the SHA-256 of the whole body is wantSHA and
// the manifest names the kernel it was given, by name, size and digest.
func receive(body io.Reader, max int64, wantSHA string, kernel io.Writer) (Manifest, error) {
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
	switch _, err := tr.Next(); {
	case err == nil:
		return Manifest{}, fmt.Errorf("%w: a member follows %s", ErrBundle, bundleManifest)
	case !errors.Is(err, io.EOF):
		return Manifest{}, readError("the end of the bundle", err)
	}
	// What follows the end blocks of the tar is part of what the digest covers.
	if _, err := io.Copy(io.Discard, src); err != nil {
		return Manifest{}, readError("the end of the bundle", err)
	}

	if got := hex.EncodeToString(whole.Sum(nil)); got != strings.ToLower(wantSHA) {
		return Manifest{}, fmt.Errorf("%w: the SHA-256 of the body is %s, not the %s asked for", ErrBundle, got, wantSHA)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %s is not JSON: %v", ErrBundle, bundleManifest, err)
	}
	if m.Version == "" {
		return Manifest{}, fmt.Errorf("%w: %s names no version", ErrBundle, bundleManifest)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); m.Kernel.Name != bundleKernel || m.Kernel.Bytes != n || n == 0 || m.Kernel.SHA256 != got {
		return Manifest{}, fmt.Errorf("%w: %s does not describe the kernel (it is %d bytes with SHA-256 %s)", ErrBundle, bundleManifest, n, got)
	}
	return m, nil
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
