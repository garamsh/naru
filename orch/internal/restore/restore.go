// Package restore turns a plain etcd snapshot into the data directory of a new member.
package restore

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.etcd.io/etcd/client/pkg/v3/types"
	"go.etcd.io/etcd/etcdutl/v3/snapshot"
	"go.uber.org/zap"
)

// clusterToken is the token embed gives a cluster that names none.
const clusterToken = "etcd-cluster"

// ErrRejected is returned when the snapshot is not one that can be restored: its SHA-256 differs
// from the one the caller sent, or etcd's own check of it fails.
var ErrRejected = errors.New("the snapshot is rejected")

// Config is the member the snapshot is restored into.
type Config struct {
	Name           string
	DataDir        string
	PeerURL        string
	InitialCluster string
}

// Check reports a configuration a restore could not use, before any snapshot is read.
func (c Config) Check() error {
	if _, err := types.NewURLs([]string{c.PeerURL}); err != nil {
		return fmt.Errorf("peer URL: %w", err)
	}
	cluster, err := types.NewURLsMap(c.InitialCluster)
	if err != nil {
		return fmt.Errorf("initial cluster: %w", err)
	}
	if _, ok := cluster[c.Name]; !ok {
		return fmt.Errorf("initial cluster %q does not name this member, %q", c.InitialCluster, c.Name)
	}
	return nil
}

// Apply reads the snapshot from r and restores it into c.DataDir. It is refused with ErrRejected
// when the SHA-256 of what was read is not wantSHA256 (hex) or etcd's check of the snapshot's own
// hash fails. The restore is made beside DataDir and renamed into place, so a failed restore leaves
// DataDir as it was.
func Apply(c Config, r io.Reader, wantSHA256 string, log *zap.Logger) error {
	want, err := hex.DecodeString(wantSHA256)
	if err != nil || len(want) != sha256.Size {
		return fmt.Errorf("%w: the SHA-256 is not 64 hex digits", ErrRejected)
	}
	parent := filepath.Dir(c.DataDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(parent, ".restore-*.snap")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	sum := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, sum), r); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(sum.Sum(nil), want) != 1 {
		return fmt.Errorf("%w: its SHA-256 is %x, not the one sent", ErrRejected, sum.Sum(nil))
	}
	staging, err := os.MkdirTemp(parent, ".restore-*.etcd")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	// Restore wants a directory that does not exist yet.
	dir := filepath.Join(staging, "data")
	if err := snapshot.NewV3(log).Restore(snapshot.RestoreConfig{
		SnapshotPath: file.Name(), Name: c.Name, OutputDataDir: dir,
		PeerURLs: []string{c.PeerURL}, InitialCluster: c.InitialCluster, InitialClusterToken: clusterToken,
	}); err != nil {
		return fmt.Errorf("%w: %v", ErrRejected, err)
	}
	return os.Rename(dir, c.DataDir)
}
