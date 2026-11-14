// Package signer is naru-signer (docs/architecture/signer.md): the process that alone holds the
// authority keys of naru's identities, issues certificates against signed requests and
// admission statements, and keeps the revocation list.
package signer

import (
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"syscall"
	"time"
)

var (
	// ErrDirectory is returned when the signer's directory is not one only the signer's user may use.
	ErrDirectory = errors.New("signer directory is not usable")
	// ErrNotInitialised is returned by a call that needs the authorities, on a directory that holds none.
	ErrNotInitialised = errors.New("signer holds no authorities")
	// ErrInitialised is returned by a call that creates the authorities, on a directory that holds them.
	ErrInitialised = errors.New("signer already holds authorities")
	// ErrForbidden is returned when a caller's role does not allow the call.
	ErrForbidden = errors.New("the caller's role does not allow this")
	// ErrBadRequest is returned when a request is well signed but names something the signer cannot do.
	ErrBadRequest = errors.New("bad request")
)

// Options is what Open reads.
type Options struct {
	// Dir is the signer's directory. It must be mode 0700 and owned by the user running the signer.
	Dir string
	Log *slog.Logger
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Signer is the signer's state: its directory and, once initialised, the authorities.
type Signer struct {
	dir string
	log *slog.Logger
	now func() time.Time

	mu   sync.RWMutex
	auth *authorities
	// peered and lastPush are guarded by mu: whether orch's sync has ever pushed a list here and
	// when it last did.
	peered   bool
	lastPush time.Time
}

// Open opens the signer's directory. A directory that holds no authority is blank: Init, Restore
// and Join fill it. A directory that holds some but not all is refused.
func Open(opts Options) (*Signer, error) {
	if err := checkDirectory(opts.Dir); err != nil {
		return nil, err
	}
	s := &Signer{dir: opts.Dir, log: opts.Log, now: opts.Now, peered: peeredIn(opts.Dir)}
	if s.now == nil {
		s.now = time.Now
	}
	a, err := loadAuthorities(opts.Dir)
	if err != nil {
		return nil, err
	}
	s.auth = a
	return s, nil
}

// Initialised reports whether the signer holds its authorities.
func (s *Signer) Initialised() bool {
	return s.snapshot() != nil
}

// snapshot returns the authorities, which are replaced as a whole and never changed in place.
func (s *Signer) snapshot() *authorities {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auth
}

func (s *Signer) ready() (*authorities, error) {
	a := s.snapshot()
	if a == nil {
		return nil, ErrNotInitialised
	}
	return a, nil
}

// checkDirectory refuses a path that is not a directory of mode 0700 owned by the running user.
func checkDirectory(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrDirectory, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrDirectory, dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("%w: %s has mode %04o, not 0700", ErrDirectory, dir, perm)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%w: %s is owned by uid %d, not the signer's %d", ErrDirectory, dir, st.Uid, os.Geteuid())
	}
	return nil
}

// pool returns a pool holding cert.
func pool(cert *x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(cert)
	return p
}
