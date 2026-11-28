package identity

import (
	"crypto/tls"
	"sync"
)

// FilePair is a certificate and its key read from files, and read again when either changes, so a
// renewed certificate is presented from the next handshake on (ADR 0123).
type FilePair struct {
	cert, key string

	mu   sync.Mutex
	pair *tls.Certificate
	// stat is the size and modification time of both files when pair was read: while they stand,
	// a handshake reads neither.
	stat [2]fileStat
}

// NewFilePair reads the pair in cert and key, which must be one.
func NewFilePair(cert, key string) (*FilePair, error) {
	p := &FilePair{cert: cert, key: key}
	if _, err := p.Pair(); err != nil {
		return nil, err
	}
	return p, nil
}

// Pair returns the pair as the files stand. It reads them only when the size or the modification
// time of one has changed since the last read. Files that cannot be read, or are not a pair, keep
// the pair last read and return the error; before any read there is none.
func (p *FilePair) Pair() (*tls.Certificate, error) {
	stat := [2]fileStat{statOf(p.cert), statOf(p.key)}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pair != nil && stat == p.stat {
		return p.pair, nil
	}
	pair, err := tls.LoadX509KeyPair(p.cert, p.key)
	if err != nil {
		return p.pair, err
	}
	p.pair, p.stat = &pair, stat
	return p.pair, nil
}

// GetCertificate is Pair for a server's handshake.
func (p *FilePair) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return p.current()
}

// GetClientCertificate is Pair for a client's handshake.
func (p *FilePair) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return p.current()
}

// current is the pair last read when the files no longer are one, so a torn write does not end
// the handshakes a good pair would make.
func (p *FilePair) current() (*tls.Certificate, error) {
	pair, err := p.Pair()
	if pair != nil {
		return pair, nil
	}
	return nil, err
}
