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

// Mount mounts each extension name of specs read-only under its directory. A name may have several
// entries, one per digest, each for another naru version (ADR 0055): for each name it mounts the
// entry whose stored image was built for init's version, so an upgrade and a rollback each find
// their own. It checks that the digest holds and checks the manifest again from the mounted image.
// It stops at the first name it cannot mount, and the error says why: no entry has an image built
// for this version, two have, a digest that does not hold, or a manifest that disagrees. An entry
// whose image is stored for another version, or not stored, is logged and skipped.
func (m *Manager) Mount(specs []config.Extension) error {
	if err := m.store.Sweep(); err != nil {
		m.log.Warn("remove the leftovers of an unfinished extension upload", "err", err)
	}
	var names []string
	for _, e := range specs {
		if !slices.Contains(names, e.Name) {
			names = append(names, e.Name)
		}
	}
	for _, name := range names {
		var entries []config.Extension
		for _, e := range specs {
			if e.Name == name {
				entries = append(entries, e)
			}
		}
		if err := m.mount(name, entries); err != nil {
			return fmt.Errorf("extension %q: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) mount(name string, entries []config.Extension) error {
	img, err := m.pick(name, entries)
	if err != nil {
		return err
	}
	f, _, err := m.store.Open(img.SHA256)
	if err != nil {
		return fmt.Errorf("image %s: %w", img.SHA256, err)
	}
	defer f.Close()
	dir := filepath.Join(m.root, name)
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
	m.log.Info("extension mounted", "name", name, "sha256", img.SHA256, "version", img.Version, "dir", dir)
	return nil
}

// pick returns the image of name that init mounts: the one entry whose stored image was built for
// init's version. Entries for other versions, or whose image is not stored, are logged and skipped.
func (m *Manager) pick(name string, entries []config.Extension) (Image, error) {
	stored, err := m.store.List()
	if err != nil {
		return Image{}, err
	}
	var found []Image
	var elsewhere []string
	for _, e := range entries {
		if e.SHA256 == "" {
			// An unpinned entry takes every stored image of its name built for init's version.
			for _, img := range stored {
				if img.Name == name && img.Version == m.version && !slices.ContainsFunc(found, func(f Image) bool { return f.SHA256 == img.SHA256 }) {
					found = append(found, img)
				}
			}
			if !slices.ContainsFunc(stored, func(img Image) bool { return img.Name == name && img.Version == m.version }) {
				for _, img := range stored {
					if img.Name == name {
						elsewhere = append(elsewhere, img.Version)
					}
				}
			}
			continue
		}
		i := slices.IndexFunc(stored, func(img Image) bool { return img.SHA256 == e.SHA256 })
		switch {
		case i < 0:
			m.log.Info("extension entry skipped: its image is not stored", "name", name, "sha256", e.SHA256)
		case stored[i].Name != name:
			return Image{}, fmt.Errorf("the image %s is the extension %q, not %q", e.SHA256, stored[i].Name, name)
		case stored[i].Version != m.version:
			elsewhere = append(elsewhere, stored[i].Version)
			m.log.Info("extension entry skipped: its image is built for another naru version", "name", name, "sha256", e.SHA256, "version", stored[i].Version)
		default:
			found = append(found, stored[i])
		}
	}
	switch {
	case len(found) == 1:
		return found[0], nil
	case len(found) > 1:
		return Image{}, fmt.Errorf("%d entries are built for naru %s, and only one may be", len(found), m.version)
	case len(elsewhere) > 0:
		return Image{}, fmt.Errorf("no entry is built for naru %s (stored for: %s)", m.version, strings.Join(elsewhere, ", "))
	}
	if entries[0].SHA256 == "" {
		return Image{}, fmt.Errorf("no image of %q is stored; an upgrade bundle or PUT /v1/extensions delivers it", name)
	}
	return Image{}, fmt.Errorf("no image of an entry is stored; PUT /v1/extensions/%s delivers it", entries[0].SHA256)
}

// ErrStage is an image of a bundle that is not the extension, or not built for the version, it was
// to be.
var ErrStage = errors.New("the bundle's extension is not what its manifest says")

// Stage stores the image read from body, whose SHA-256 must be digest, and checks that its manifest
// names name and the naru version the bundle is for (ADR 0084). An image that fails the check and
// was not stored before is removed again. added tells whether the image is new to the store.
func (m *Manager) Stage(body io.Reader, digest, name, version string) (img Image, added bool, err error) {
	stored, _ := m.store.List()
	added = !slices.ContainsFunc(stored, func(i Image) bool { return i.SHA256 == digest })
	img, err = m.store.Put(body, digest)
	if err != nil {
		return Image{}, false, err
	}
	if img.Name != name || img.Version != version {
		if added {
			_ = m.store.Delete(digest)
		}
		return Image{}, false, fmt.Errorf("%w: %s holds %q for naru %s, not %q for %s", ErrStage, digest, img.Name, img.Version, name, version)
	}
	return img, added, nil
}

// Prune removes the stored images that no pinned entry of specs names, whose name an unpinned entry
// lists, and that are built for another version than init's. It returns their digests. A pinned
// image is the operator's and stays (ADR 0084).
func (m *Manager) Prune(specs []config.Extension) ([]string, error) {
	stored, err := m.store.List()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, img := range stored {
		if img.Version == m.version || !m.prunable(img, specs) {
			continue
		}
		if err := m.Delete(img.SHA256); err != nil {
			if errors.Is(err, ErrMounted) {
				continue
			}
			return removed, err
		}
		removed = append(removed, img.SHA256)
		m.log.Info("extension image removed: built for another naru version than this", "name", img.Name, "sha256", img.SHA256, "version", img.Version)
	}
	return removed, nil
}

func (m *Manager) prunable(img Image, specs []config.Extension) bool {
	unpinned := false
	for _, e := range specs {
		switch {
		case e.SHA256 == img.SHA256:
			return false
		case e.SHA256 == "" && e.Name == img.Name:
			unpinned = true
		}
	}
	return unpinned
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
