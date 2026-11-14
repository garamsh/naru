// Package snapshot takes etcd snapshots, seals each to the recovery key and keeps the newest few.
package snapshot

import (
	"context"
	"crypto/ecdh"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

// Defaults of the schedule and retention.
const (
	DefaultInterval = 6 * time.Hour
	DefaultKeep     = 8
)

const (
	filePrefix = "etcd-"
	fileSuffix = ".snap.sealed"
)

// ErrNoSealer is returned when no recovery key is configured. A snapshot holds the whole
// inventory, so it is never written unsealed.
var ErrNoSealer = errors.New("no recovery public key is configured, so no snapshot is written")

// ErrNoSnapshot is returned when the directory holds no sealed snapshot yet.
var ErrNoSnapshot = errors.New("no snapshot has been written yet")

// Sealer seals data to the recovery key's public half.
type Sealer interface {
	Seal(plain []byte) ([]byte, error)
}

// Info binds a sealed snapshot to its purpose: lib/identity's Open fails under any other.
const Info = orchapi.SnapshotInfo

// SealTo returns a Sealer that seals with lib/identity's sealed box to the recovery key's X25519
// public key.
func SealTo(pub *ecdh.PublicKey) Sealer { return boxSealer{pub} }

type boxSealer struct{ pub *ecdh.PublicKey }

func (b boxSealer) Seal(plain []byte) ([]byte, error) {
	return identity.Seal(b.pub, plain, []byte(Info))
}

// Source streams a snapshot of the database.
type Source interface {
	Snapshot(ctx context.Context) (io.ReadCloser, error)
}

// Taker writes sealed snapshots into Dir.
type Taker struct {
	Source Source
	Sealer Sealer
	Dir    string
	Keep   int
	Now    func() time.Time
	Log    *slog.Logger
}

// ClientSource adapts an etcd client to Source.
func ClientSource(cl *clientv3.Client) Source { return clientSource{cl} }

type clientSource struct{ cl *clientv3.Client }

func (c clientSource) Snapshot(ctx context.Context) (io.ReadCloser, error) {
	return c.cl.Snapshot(ctx)
}

// Take writes a sealed snapshot and prunes the directory to the newest Keep.
func (t *Taker) Take(ctx context.Context) (orchapi.SnapshotResult, error) {
	if t.Sealer == nil {
		return orchapi.SnapshotResult{}, ErrNoSealer
	}
	rc, err := t.Source.Snapshot(ctx)
	if err != nil {
		return orchapi.SnapshotResult{}, fmt.Errorf("read the snapshot: %w", err)
	}
	plain, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return orchapi.SnapshotResult{}, fmt.Errorf("read the snapshot: %w", err)
	}
	sealed, err := t.Sealer.Seal(plain)
	if err != nil {
		return orchapi.SnapshotResult{}, fmt.Errorf("seal the snapshot: %w", err)
	}
	if err := os.MkdirAll(t.Dir, 0o700); err != nil {
		return orchapi.SnapshotResult{}, err
	}
	name := filePrefix + t.Now().UTC().Format("20060102T150405.000000000Z") + fileSuffix
	tmp, err := os.CreateTemp(t.Dir, ".snap-*")
	if err != nil {
		return orchapi.SnapshotResult{}, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(sealed); err != nil {
		_ = tmp.Close()
		return orchapi.SnapshotResult{}, err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return orchapi.SnapshotResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return orchapi.SnapshotResult{}, err
	}
	if err := tmp.Close(); err != nil {
		return orchapi.SnapshotResult{}, err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(t.Dir, name)); err != nil {
		return orchapi.SnapshotResult{}, err
	}
	return orchapi.SnapshotResult{File: name, Size: int64(len(sealed))}, t.prune()
}

// Latest returns the newest sealed snapshot in Dir.
func (t *Taker) Latest() ([]byte, error) {
	names, err := List(t.Dir)
	if errors.Is(err, os.ErrNotExist) || err == nil && len(names) == 0 {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(t.Dir, names[0]))
}

func (t *Taker) prune() error {
	names, err := List(t.Dir)
	if err != nil {
		return err
	}
	keep := t.Keep
	if keep <= 0 {
		keep = DefaultKeep
	}
	for i := keep; i < len(names); i++ {
		if err := os.Remove(filepath.Join(t.Dir, names[i])); err != nil {
			return err
		}
	}
	return nil
}

// List returns the snapshot file names in dir, newest first.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, filePrefix) && strings.HasSuffix(n, fileSuffix) {
			names = append(names, n)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, nil
}

// Run takes a snapshot every interval until ctx ends.
func (t *Taker) Run(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := t.Take(ctx); err != nil && ctx.Err() == nil {
				t.Log.Error("scheduled snapshot failed", "err", err.Error())
			}
		}
	}
}
