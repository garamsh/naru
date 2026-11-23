package extension

import (
	"errors"
	"io"
	"slices"

	"github.com/garamsh/naru/lib/init/config"
)

// ErrNoOffer is what a Source returns when it holds no image of the name asked for.
var ErrNoOffer = errors.New("the source holds no image of that name")

// Offer is an image a Source holds: the SHA-256 and the naru version it says it was built for, and
// the bytes. Init checks all three as it stages the image, so a source is not trusted.
type Offer struct {
	SHA256  string
	Version string
	Body    io.ReadCloser
}

// Source is a place that came with the Node and holds extension images of the release the Node was
// installed from: the ESP of its boot disk, the boot set it was started from (ADR 0096). Init
// takes from it only the image of an unpinned entry that the Node's own store lacks.
type Source interface {
	// Name says what the source is, for the log.
	Name() string
	// Offer returns the image of name, or ErrNoOffer. The caller closes the body.
	Offer(name string) (Offer, error)
}

// Provide stores, for each unpinned entry of specs whose image built for init's version is not in
// the store, the image the first of sources that holds one offers (ADR 0096). The store is the
// Node's own, so an image it holds, whatever put it there, is never replaced, and an entry with a
// digest is the operator's pin and is never provided. A source that fails, or offers an image for
// another version, is logged and the next is asked; Provide returns no error, since Mount, which
// runs next, refuses the boot with the reason when no image is there.
func (m *Manager) Provide(specs []config.Extension, sources []Source) {
	var done []string
	for _, e := range specs {
		if e.SHA256 != "" || slices.Contains(done, e.Name) {
			continue
		}
		done = append(done, e.Name)
		if m.has(e.Name) {
			continue
		}
		for _, src := range sources {
			if m.provide(e.Name, src) {
				break
			}
		}
	}
}

// has reports whether the store holds an image of name built for init's version.
func (m *Manager) has(name string) bool {
	stored, err := m.store.List()
	if err != nil {
		return false
	}
	return slices.ContainsFunc(stored, func(img Image) bool { return img.Name == name && img.Version == m.version })
}

// provide takes the image of name from src into the store and reports whether it did.
func (m *Manager) provide(name string, src Source) bool {
	off, err := src.Offer(name)
	if errors.Is(err, ErrNoOffer) {
		return false
	}
	if err != nil {
		m.log.Warn("extension source failed", "source", src.Name(), "name", name, "err", err)
		return false
	}
	defer off.Body.Close()
	if off.Version != m.version {
		m.log.Info("extension source skipped: its image is built for another naru version", "source", src.Name(), "name", name, "version", off.Version)
		return false
	}
	img, _, err := m.Stage(off.Body, off.SHA256, name, m.version)
	if err != nil {
		m.log.Warn("extension image from the source refused", "source", src.Name(), "name", name, "err", err)
		return false
	}
	m.log.Info("extension staged from the boot medium", "source", src.Name(), "name", name, "sha256", img.SHA256, "version", img.Version)
	return true
}
