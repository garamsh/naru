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

	"github.com/garamsh/naru/init/internal/config"
)

// Dir is where a Node keeps the store, on the data disk.
const Dir = "/var/lib/naru/config"

// FileName is the store's file in its directory.
const FileName = "store.yaml"

// ErrNotFound is returned by Load when the directory holds no store.
var ErrNotFound = errors.New("no stored configuration")

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
// ErrNotFound when there is no store.
func (s *Store) Load() (Stored, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return Stored{}, ErrNotFound
	}
	if err != nil {
		return Stored{}, err
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

// Save writes the store atomically: a temporary file in the directory, fsync,
// rename, and fsync of the directory. A failure leaves the previous store.
func (s *Store) Save(st Stored) (err error) {
	data, err := yaml.Dump(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".store-*")
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
	if err = os.Rename(f.Name(), filepath.Join(s.dir, FileName)); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}
