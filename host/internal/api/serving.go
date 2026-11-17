package api

import (
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/garamsh/naru/lib/atomicfile"
)

// Serving is the pair the host API presents to its clients. It starts as the pair of the
// configuration and is replaced live by Replace, which also stores the new pair so a restart serves it
// (ADR 0077).
type Serving struct {
	stored string
	mu     sync.RWMutex
	pair   tls.Certificate
	file   string
}

// NewServing loads the pair in certFile and keyFile. stored is where Replace keeps a replacement:
// one file holding the certificate and the key, written 0600 in one rename, so a crash leaves the pair
// that was there or the new one.
func NewServing(certFile, keyFile, stored string) (*Serving, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return &Serving{stored: stored, pair: pair, file: certFile}, nil
}

// File is the file whose certificate is being served: the stored file after a Replace.
func (s *Serving) File() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.file
}

// GetCertificate is the tls.Config's callback.
func (s *Serving) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return &s.pair, nil
}

// Replace stores the PEM certificate and key and serves them from the next handshake on. A pair that
// does not match is refused with nothing changed.
func (s *Serving) Replace(certPEM, keyPEM []byte) error {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	if s.stored == "" {
		return errors.New("no file to keep the serving pair in")
	}
	if err := os.MkdirAll(filepath.Dir(s.stored), 0o755); err != nil {
		return err
	}
	both := append(append([]byte{}, certPEM...), keyPEM...)
	if err := atomicfile.Write(s.stored, both, 0o600); err != nil {
		return err
	}
	s.mu.Lock()
	s.pair, s.file = pair, s.stored
	s.mu.Unlock()
	return nil
}
