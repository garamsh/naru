// Package store keeps a Node's effective configuration, its revision and the
// digest of the base it came from, in one file on the data disk
// (docs/architecture/adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md).
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v4"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/wire"
)

// Dir is where a Node keeps the store, on the data disk.
const Dir = wire.DataMount + "/config"

// FileName is the store's file in its directory.
const FileName = "store.yaml"

// ErrNotFound is returned by Load when the directory holds no store.
var ErrNotFound = errors.New("no stored configuration")

// ErrUnreadable is returned by Load when the store exists and cannot be read. The store is
// intact as far as the caller knows, unlike one that was read and does not validate.
var ErrUnreadable = errors.New("the stored configuration cannot be read")

// Stored is the effective configuration of a Node.
type Stored struct {
	// Revision is 0 for the base, and one more for each apply.
	Revision int `yaml:"revision"`
	// BaseSha256 is the hex SHA-256 of the user-data the base was.
	BaseSha256 string `yaml:"baseSha256"`
	// Config is the effective configuration.
	Config config.NodeConfig `yaml:"config"`
}

// Digest returns the hex SHA-256 of the user-data bytes.
func Digest(userData []byte) string {
	sum := sha256.Sum256(userData)
	return hex.EncodeToString(sum[:])
}

// Store is the store in one directory.
type Store struct{ dir string }

// New returns the store in dir.
func New(dir string) *Store { return &Store{dir: dir} }

// Load reads the store and parses its configuration with config.Parse, so a
// stored configuration that does not validate is an error. It returns
// ErrNotFound when there is no store and an error wrapping ErrUnreadable when reading it fails.
func (s *Store) Load() (Stored, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return Stored{}, ErrNotFound
	}
	if err != nil {
		return Stored{}, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	var raw struct {
		Revision   int       `yaml:"revision"`
		BaseSha256 string    `yaml:"baseSha256"`
		Config     yaml.Node `yaml:"config"`
	}
	if err := yaml.Load(data, &raw, yaml.WithKnownFields()); err != nil {
		return Stored{}, fmt.Errorf("parse %s: %w", FileName, err)
	}
	if raw.Revision < 0 {
		return Stored{}, fmt.Errorf("%s: revision is %d", FileName, raw.Revision)
	}
	doc, err := yaml.Dump(&raw.Config)
	if err != nil {
		return Stored{}, fmt.Errorf("%s: %w", FileName, err)
	}
	cfg, err := config.Parse(doc)
	if err != nil {
		return Stored{}, fmt.Errorf("%s: %w", FileName, err)
	}
	return Stored{Revision: raw.Revision, BaseSha256: raw.BaseSha256, Config: cfg}, nil
}

// Save writes the store atomically with atomicfile.Write. A failure leaves the previous store. A store that exists
// and cannot be read is not written over: Save returns an error wrapping ErrUnreadable, since
// the revisions in it are unknown.
func (s *Store) Save(st Stored) error {
	if _, err := os.ReadFile(filepath.Join(s.dir, FileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %w; a reboot or repair of the data disk is needed", ErrUnreadable, err)
	}
	data, err := yaml.Dump(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(s.dir, FileName), data, 0o600)
}
