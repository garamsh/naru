package extension

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/garamsh/naru/lib/init/config"
)

// Entry is a stored extension and whether init has it mounted.
type Entry struct {
	Image
	Mounted bool `json:"mounted"`
	// MountPoint is where it is mounted, and absent when it is not.
	MountPoint string `json:"mountPoint,omitempty"`
}

// Manager mounts the extensions spec.extensions names at boot and serves the store to the Node
// API.
type Manager struct {
	store   *Store
	mounter Mounter
	root    string
	version string
	log     *slog.Logger

	mu      sync.Mutex
	mounted map[string]string // digest -> mount point
}

// NewManager returns a Manager over store that mounts through mounter, each extension under root
// as root/<name>, the images built for version, which is init's own.
func NewManager(store *Store, mounter Mounter, root, version string, log *slog.Logger) *Manager {
	return &Manager{store: store, mounter: mounter, root: root, version: version, log: log, mounted: map[string]string{}}
}

// Mount mounts each extension of specs read-only under its directory. For each name it picks the
// stored image whose manifest names it and was built for init's version, checks that the image is
// the one the configuration names and that its digest holds, and checks the manifest again from the
// mounted image. It stops at the first extension it cannot mount, and the error says why: a missing
// image, one built for another version, a digest that does not hold, or a manifest that disagrees.
func (m *Manager) Mount(specs []config.Extension) error {
	if err := m.store.Sweep(); err != nil {
		m.log.Warn("remove the leftovers of an unfinished extension upload", "err", err)
	}
	for _, e := range specs {
		if err := m.mount(e); err != nil {
			return fmt.Errorf("extension %q: %w", e.Name, err)
		}
	}
	return nil
}

func (m *Manager) mount(e config.Extension) error {
	img, err := m.pick(e)
	if err != nil {
		return err
	}
	f, _, err := m.store.Open(img.SHA256)
	if err != nil {
		return fmt.Errorf("image %s: %w", img.SHA256, err)
	}
	defer f.Close()
	dir := filepath.Join(m.root, e.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := m.mounter.Mount(f, dir); err != nil {
		return fmt.Errorf("mount image %s on %s: %w", img.SHA256, dir, err)
	}
	got, err := ReadManifest(dir)
	if err == nil && (got.Name != img.Name || got.Version != img.Version || !slices.Equal(got.Services, img.Services)) {
		err = fmt.Errorf("the manifest inside the image differs from the one stored with it")
	}
	if err != nil {
		if uerr := m.mounter.Unmount(dir); uerr != nil {
			err = errors.Join(err, uerr)
		}
		return fmt.Errorf("image %s: %w", img.SHA256, err)
	}
	m.mu.Lock()
	m.mounted[img.SHA256] = dir
	m.mu.Unlock()
	m.log.Info("extension mounted", "name", e.Name, "sha256", img.SHA256, "version", img.Version, "dir", dir)
	return nil
}

// pick returns the stored image of e.Name built for init's version, which must be the image e.SHA256 names.
func (m *Manager) pick(e config.Extension) (Image, error) {
	stored, err := m.store.List()
	if err != nil {
		return Image{}, err
	}
	var versions []string
	var built *Image
	for _, img := range stored {
		if img.Name != e.Name {
			if img.SHA256 == e.SHA256 {
				return Image{}, fmt.Errorf("the image %s is the extension %q, not %q", e.SHA256, img.Name, e.Name)
			}
			continue
		}
		versions = append(versions, img.Version)
		if img.Version == m.version {
			built = &img
			if img.SHA256 == e.SHA256 {
				return img, nil
			}
		}
	}
	switch {
	case built != nil:
		return Image{}, fmt.Errorf("the image stored for naru %s has SHA-256 %s, not the configured %s", m.version, built.SHA256, e.SHA256)
	case slices.ContainsFunc(stored, func(i Image) bool { return i.SHA256 == e.SHA256 }):
		return Image{}, fmt.Errorf("the image %s is not built for naru %s (stored for: %s)", e.SHA256, m.version, strings.Join(versions, ", "))
	case len(versions) > 0:
		return Image{}, fmt.Errorf("no image %s is stored; stored images are built for naru %s, and this is naru %s", e.SHA256, strings.Join(versions, ", "), m.version)
	}
	return Image{}, fmt.Errorf("no image is stored; PUT /v1/extensions/%s delivers it", e.SHA256)
}

// Put stores the image read from body, whose SHA-256 must be digest.
func (m *Manager) Put(body io.Reader, digest string) (Entry, error) {
	img, err := m.store.Put(body, digest)
	if err != nil {
		return Entry{}, err
	}
	return m.entry(img), nil
}

// Delete removes the stored image unless it is mounted.
func (m *Manager) Delete(digest string) error {
	m.mu.Lock()
	_, mounted := m.mounted[digest]
	m.mu.Unlock()
	if mounted {
		return ErrMounted
	}
	return m.store.Delete(digest)
}

// List returns every stored extension, ordered by digest.
func (m *Manager) List() ([]Entry, error) {
	stored, err := m.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(stored))
	for _, img := range stored {
		out = append(out, m.entry(img))
	}
	return out, nil
}

func (m *Manager) entry(img Image) Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	dir, ok := m.mounted[img.SHA256]
	return Entry{Image: img, Mounted: ok, MountPoint: dir}
}
