package api

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/garamsh/naru/lib/identity"
)

// ClientPool is the pool of authorities hostd verifies its callers and its peers through: the
// --client-ca file and the file of re-signings the agent stored from orch (ADR 0122), which lives
// on the data disk because init writes --client-ca afresh at each boot. Both are read again when
// either changes, so a refreshed authority is trusted from the next handshake on.
type ClientPool struct {
	base, refreshed string

	mu   sync.Mutex
	seen []byte
	pool *x509.CertPool
}

// NewClientPool reads base and refreshed, which together must hold a certificate. Refreshed may be
// absent, or the empty path: a host the agent has not refreshed yet trusts base alone.
func NewClientPool(base, refreshed string) (*ClientPool, error) {
	p := &ClientPool{base: base, refreshed: refreshed}
	if _, err := p.Pool(); err != nil {
		return nil, err
	}
	return p, nil
}

// PEM returns both files, base first. A refreshed file that is absent adds nothing.
func (p *ClientPool) PEM() ([]byte, error) {
	b, err := os.ReadFile(p.base)
	if err != nil {
		return nil, err
	}
	if p.refreshed == "" {
		return b, nil
	}
	r, err := os.ReadFile(p.refreshed)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return append(append(append([]byte{}, b...), '\n'), r...), nil
}

// Pool returns the pool of both files as they stand. A pair of files that cannot be read, or holds
// no certificate, keeps the pool last read and returns the error; before any read there is none.
func (p *ClientPool) Pool() (*x509.CertPool, error) {
	b, err := p.PEM()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil && p.pool != nil && bytes.Equal(b, p.seen) {
		return p.pool, nil
	}
	if err == nil {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(b) {
			p.seen, p.pool = b, pool
			return pool, nil
		}
		err = fmt.Errorf("%s holds no certificate", p.base)
	}
	return p.pool, err
}

// current is Pool for a handshake, which needs a pool whatever went wrong since the last read.
func (p *ClientPool) current() *x509.CertPool {
	pool, _ := p.Pool()
	return pool
}

// TLSConfig is the host API's TLS configuration: it serves the pair serving holds and verifies a
// client's certificate against the pool, read again at each handshake when its files changed, and
// the revocation list when one is given.
func TLSConfig(serving *Serving, pool *ClientPool, revocations *identity.RevocationList) *tls.Config {
	base := ServerTLS(serving.GetCertificate, pool.current(), revocations)
	conf := base.Clone()
	conf.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		c := base.Clone()
		c.ClientCAs = pool.current()
		return c, nil
	}
	return conf
}
