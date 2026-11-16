// Package store keeps a Node's effective configuration, its revision and the
// digest of the base it came from, in one file on the data disk
// (docs/architecture/adr/0018-a-nodes-configuration-is-a-base-and-numbered-revisions-stored-on-the-data-disk-and-changed-by-merge-patch.md).
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// NetworkFileName is the network-config of the seed the store was last taken from, in the same
// directory.
const NetworkFileName = "network-config"

// ResetFileName is the file naru-hostd writes to ask the next boot for a reset (ADR 0082), in the
// same directory. Its path is the --reset-marker flag's default in naru-hostd.
const ResetFileName = "reset.json"

// AnchorFileName is the maintenance anchor, in the same directory: what init keeps of the trust
// a refused host's maintenance mode is served under (ADR 0082).
const AnchorFileName = "maintenance.json"

// The scopes of a reset (ADR 0082). They are the strings of lib/hostapi's reset call.
const (
	ScopeStore = "store"
	ScopeData  = "data"
	ScopeAll   = "all"
)

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

// SaveNetwork writes the network-config of the seed, which may be empty, beside the store with
// atomicfile.Write, so that a boot with no seed brings the network up as the last one with a seed did.
func (s *Store) SaveNetwork(networkConfig []byte) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(s.dir, NetworkFileName), networkConfig, 0o600)
}

// LoadNetwork returns the network-config SaveNetwork wrote, and ErrNotFound when none was written.
func (s *Store) LoadNetwork() ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, NetworkFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	return data, nil
}

// Discard removes the stored configuration, so that the next boot runs its base as revision 0, and
// leaves the network-config and everything else beside it. It is not an error when there is none.
func (s *Store) Discard() error {
	err := os.Remove(filepath.Join(s.dir, FileName))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// TakeReset reads the reset the host asked for and removes the request before anything is done,
// so that a boot that fails half way is not asked again. It returns false when there is none. A
// request that is not a reset of a known scope is removed and returned as an error.
func (s *Store) TakeReset() (scope string, found bool, err error) {
	path := filepath.Join(s.dir, ResetFileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if err := os.Remove(path); err != nil {
		return "", false, err
	}
	var req struct {
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return "", true, fmt.Errorf("%s: %w", ResetFileName, err)
	}
	switch req.Scope {
	case ScopeStore, ScopeData, ScopeAll:
		return req.Scope, true, nil
	}
	return "", true, fmt.Errorf("%s: unknown scope %q", ResetFileName, req.Scope)
}

// SaveAnchor writes the maintenance anchor beside the store with atomicfile.Write.
func (s *Store) SaveAnchor(anchor []byte) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(s.dir, AnchorFileName), anchor, 0o600)
}

// LoadAnchor returns what SaveAnchor wrote, and ErrNotFound when none was written.
func (s *Store) LoadAnchor() ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, AnchorFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnreadable, err)
	}
	return data, nil
}
