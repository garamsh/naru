// Package extension keeps the system extensions of a Node on its data disk and mounts them at boot
// (docs/architecture/adr/0055-orch-and-the-signer-are-delivered-as-a-system-extension-a-squashfs-image-init-mounts-read-only.md).
package extension

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Dir is where a Node keeps its extension images, on the data disk, beside the configuration store.
const Dir = "/var/lib/naru/extensions"

// MaxBytes is the largest image a Node takes.
const MaxBytes = 1 << 30

// ManifestPath is the manifest's path inside an image.
const ManifestPath = "manifest.json"

const (
	imageSuffix    = ".sqfs"
	manifestSuffix = ".json"
	tempPrefix     = ".put-"
)

var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The errors Put, Delete and Open return.
var (
	// ErrDigest means the image's SHA-256 is not the one the caller gave, or the one given is not a SHA-256.
	ErrDigest = errors.New("the image does not have the SHA-256 given")
	// ErrImage means the bytes are not an extension image: they cannot be mounted, or hold no valid manifest.
	ErrImage = errors.New("not an extension image")
	// ErrTooLarge means the image is larger than the store takes.
	ErrTooLarge = errors.New("the image is too large")
	// ErrNotFound means the store holds no image with the digest.
	ErrNotFound = errors.New("no stored extension with that SHA-256")
	// ErrMounted means the image is mounted, so it cannot be removed.
	ErrMounted = errors.New("the extension is mounted")
)

// Manifest is the /manifest.json inside an image.
type Manifest struct {
	// Name is the extension's name, as spec.extensions[].name.
	Name string `json:"name"`
	// Version is the naru version the image was built for.
	Version string `json:"version"`
	// Services are the names of the services the extension supplies.
	Services []string `json:"services"`
}

func (m Manifest) valid() error {
	switch {
	case m.Name == "":
		return errors.New("the manifest has no name")
	case m.Version == "":
		return errors.New("the manifest has no version")
	case len(m.Services) == 0:
		return errors.New("the manifest names no service")
	}
	return nil
}

// Image is one stored extension.
type Image struct {
	// SHA256 is the hex SHA-256 of the image, and its file's name.
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Manifest
}

// Mounter attaches an image read-only through a loop device and mounts it, and unmounts it.
type Mounter interface {
	// Mount attaches image read-only and mounts it read-only on dir, which exists.
	Mount(image *os.File, dir string) error
	// Unmount unmounts dir and releases the device behind it.
	Unmount(dir string) error
}

// Store is the images in one directory. Each image is a file named by its SHA-256, with its
// manifest beside it, so the image of the next release can be stored while the current one is
// mounted.
type Store struct {
	dir     string
	probe   string
	max     int64
	mounter Mounter

	mu sync.Mutex
}

// NewStore returns the store in dir, which takes no image larger than max bytes. It reads a
// manifest by mounting the image on a directory under probe, which it makes.
func NewStore(dir, probe string, max int64, m Mounter) *Store {
	return &Store{dir: dir, probe: probe, max: max, mounter: m}
}

func (s *Store) imagePath(digest string) string { return filepath.Join(s.dir, digest+imageSuffix) }
func (s *Store) manifestPath(digest string) string {
	return filepath.Join(s.dir, digest+manifestSuffix)
}

// Put stores the image read from body, whose SHA-256 must be digest. It writes a temporary file
// while it hashes, checks the digest, mounts the file to read its manifest, and renames it into
// place. A failure leaves nothing behind, and an image already stored is left as it is.
func (s *Store) Put(body io.Reader, digest string) (img Image, err error) {
	if !digestRE.MatchString(digest) {
		return Image{}, fmt.Errorf("%w: %q is not a lowercase hex SHA-256", ErrDigest, digest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if have, gerr := s.get(digest); gerr == nil {
		return have, nil
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return Image{}, err
	}
	f, err := os.CreateTemp(s.dir, tempPrefix+"*")
	if err != nil {
		return Image{}, err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, s.max+1))
	if err == nil && n > s.max {
		err = ErrTooLarge
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		return Image{}, errors.Join(err, f.Close())
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		_ = f.Close()
		return Image{}, fmt.Errorf("%w: the body is %s", ErrDigest, got)
	}
	m, err := s.readManifest(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Image{}, err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return Image{}, err
	}
	if err = writeAtomic(s.manifestPath(digest), data); err != nil {
		return Image{}, err
	}
	if err = os.Rename(f.Name(), s.imagePath(digest)); err != nil {
		_ = os.Remove(s.manifestPath(digest))
		return Image{}, err
	}
	if err = syncDir(s.dir); err != nil {
		return Image{}, err
	}
	return Image{SHA256: digest, Size: n, Manifest: m}, nil
}

// readManifest mounts f on a directory of its own and reads its manifest.
func (s *Store) readManifest(f *os.File) (Manifest, error) {
	if err := os.MkdirAll(s.probe, 0o700); err != nil {
		return Manifest{}, err
	}
	dir, err := os.MkdirTemp(s.probe, "probe-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.Remove(dir)
	if err := s.mounter.Mount(f, dir); err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrImage, err)
	}
	m, rerr := ReadManifest(dir)
	if uerr := s.mounter.Unmount(dir); uerr != nil {
		return Manifest{}, fmt.Errorf("unmount %s: %w", dir, uerr)
	}
	if rerr != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrImage, rerr)
	}
	return m, nil
}

// ReadManifest reads and checks the manifest of the image mounted on dir.
func ReadManifest(dir string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, ManifestPath))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestPath, err)
	}
	if err := m.valid(); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", ManifestPath, err)
	}
	return m, nil
}

// List returns the stored images ordered by digest. An image without a readable manifest is left out.
func (s *Store) List() ([]Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Image
	for _, e := range entries {
		digest, ok := strings.CutSuffix(e.Name(), imageSuffix)
		if !ok || !digestRE.MatchString(digest) {
			continue
		}
		if img, err := s.get(digest); err == nil {
			out = append(out, img)
		}
	}
	return out, nil
}

// get reads one stored image. The caller holds s.mu.
func (s *Store) get(digest string) (Image, error) {
	st, err := os.Stat(s.imagePath(digest))
	if errors.Is(err, fs.ErrNotExist) {
		return Image{}, ErrNotFound
	}
	if err != nil {
		return Image{}, err
	}
	data, err := os.ReadFile(s.manifestPath(digest))
	if err != nil {
		return Image{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Image{}, fmt.Errorf("%s: %w", s.manifestPath(digest), err)
	}
	if err := m.valid(); err != nil {
		return Image{}, fmt.Errorf("%s: %w", s.manifestPath(digest), err)
	}
	return Image{SHA256: digest, Size: st.Size(), Manifest: m}, nil
}

// Delete removes the image and its manifest.
func (s *Store) Delete(digest string) error {
	if !digestRE.MatchString(digest) {
		return fmt.Errorf("%w: %q is not a lowercase hex SHA-256", ErrDigest, digest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.imagePath(digest)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	if err := os.Remove(s.manifestPath(digest)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(s.dir)
}

// Open returns the stored image of digest open, with its manifest. It hashes the file, so an
// image that changed on the disk since Put is refused with ErrDigest.
func (s *Store) Open(digest string) (*os.File, Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	img, err := s.get(digest)
	if err != nil {
		return nil, Image{}, err
	}
	f, err := os.Open(s.imagePath(digest))
	if err != nil {
		return nil, Image{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		_ = f.Close()
		return nil, Image{}, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		_ = f.Close()
		return nil, Image{}, fmt.Errorf("%w: the stored file is %s", ErrDigest, got)
	}
	return f, img, nil
}

// Sweep removes the temporary files a Put that did not finish left.
func (s *Store) Sweep() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			errs = append(errs, os.Remove(filepath.Join(s.dir, e.Name())))
		}
	}
	return errors.Join(errs...)
}

// writeAtomic writes data to path through a temporary file and a rename.
func writeAtomic(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), tempPrefix+"*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
