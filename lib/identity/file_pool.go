package identity

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// FilePool is a pool of authorities read from files: one that must hold a certificate, and stores
// of re-signings beside it that may be absent (ADR 0122, ADR 0123). The files are read again when
// one changes, so a refreshed authority is trusted from the next handshake on.
type FilePool struct {
	files []string

	mu   sync.Mutex
	seen []byte
	pool *x509.CertPool
	// stat is the size and modification time of every file when seen was read: while they stand,
	// a handshake reads none of them.
	stat []fileStat
}

// fileStat is what a handshake compares of a file before it reads it.
type fileStat struct {
	size int64
	mod  time.Time
}

func statOf(path string) fileStat {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStat{size: -1}
	}
	return fileStat{size: fi.Size(), mod: fi.ModTime()}
}

// NewFilePool reads base and the stores, which together must hold a certificate. A store may be
// absent, and an empty path names none.
func NewFilePool(base string, stores ...string) (*FilePool, error) {
	p := &FilePool{files: []string{base}}
	for _, s := range stores {
		if s != "" {
			p.files = append(p.files, s)
		}
	}
	if _, err := p.Pool(); err != nil {
		return nil, err
	}
	return p, nil
}

// PEM returns every file, base first. A store that is absent adds nothing.
func (p *FilePool) PEM() ([]byte, error) { return FilesPEM(p.files[0], p.files[1:]...) }

// FilesPEM is base and the stores, as a FilePool of them reads them.
func FilesPEM(base string, stores ...string) ([]byte, error) {
	b, err := os.ReadFile(base)
	if err != nil {
		return nil, err
	}
	out := append([]byte{}, b...)
	for _, s := range stores {
		if s == "" {
			continue
		}
		r, err := os.ReadFile(s)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		out = append(append(out, '\n'), r...)
	}
	return out, nil
}

// Pool returns the pool of the files as they stand. It reads them only when the size or the
// modification time of one has changed since the last read, and builds a new pool only when their
// bytes have. Files that cannot be read, or hold no certificate, keep the pool last read and
// return the error; before any read there is none.
func (p *FilePool) Pool() (*x509.CertPool, error) {
	stat := make([]fileStat, len(p.files))
	for i, f := range p.files {
		stat[i] = statOf(f)
	}
	p.mu.Lock()
	if p.pool != nil && equalStats(stat, p.stat) {
		defer p.mu.Unlock()
		return p.pool, nil
	}
	p.mu.Unlock()
	b, err := p.PEM()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil && p.pool != nil && bytes.Equal(b, p.seen) {
		p.stat = stat
		return p.pool, nil
	}
	if err == nil {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(b) {
			p.seen, p.pool, p.stat = b, pool, stat
			return pool, nil
		}
		err = fmt.Errorf("%s holds no certificate", p.files[0])
	}
	return p.pool, err
}

func equalStats(a, b []fileStat) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Current is Pool for a handshake, which needs a pool whatever went wrong since the last read.
func (p *FilePool) Current() *x509.CertPool {
	pool, _ := p.Pool()
	return pool
}
