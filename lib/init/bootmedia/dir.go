package bootmedia

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/garamsh/naru/lib/init/extension"
)

// BootExtensionsDir is where the Node's initramfs carries the extension images of its release, each as
// <name>.sqfs with its description <name>.json (ADR 0096).
const BootExtensionsDir = "/usr/lib/naru/boot-extensions"

// Dir is the extension Source of a Node that booted from an initramfs that carries images: the
// files init copied to its tmpfs with the rest of the initramfs. Release frees them once staged.
type Dir struct {
	// Root is the directory that holds the images.
	Root string
}

// Name says what the source is.
func (d Dir) Name() string { return "the initramfs" }

// Offer returns the image of name that Root holds, as its description names it, or ErrNoOffer.
func (d Dir) Offer(name string) (extension.Offer, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return extension.Offer{}, extension.ErrNoOffer
	}
	raw, err := os.ReadFile(filepath.Join(d.Root, name+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return extension.Offer{}, extension.ErrNoOffer
	}
	if err != nil {
		return extension.Offer{}, err
	}
	var desc struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		SHA256  string `json:"sha256"`
	}
	if err := json.Unmarshal(raw, &desc); err != nil {
		return extension.Offer{}, fmt.Errorf("the description of %s: %w", name, err)
	}
	if desc.Name != name {
		return extension.Offer{}, fmt.Errorf("the description %s.json names %q", name, desc.Name)
	}
	f, err := os.Open(filepath.Join(d.Root, name+".sqfs"))
	if err != nil {
		return extension.Offer{}, err
	}
	return extension.Offer{SHA256: desc.SHA256, Version: desc.Version, Body: f}, nil
}

// Release removes the images, so the memory of the tmpfs they are in is free again.
func (d Dir) Release() error { return os.RemoveAll(d.Root) }
