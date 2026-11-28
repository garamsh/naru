package api

import (
	"crypto/tls"

	"github.com/garamsh/naru/lib/identity"
)

// ClientPool is the pool of authorities hostd verifies its callers and its peers through: the
// --client-ca file and the file of re-signings the agent stored from orch (ADR 0122), which lives
// on the data disk because init writes --client-ca afresh at each boot. Both are read again when
// either changes, so a refreshed authority is trusted from the next handshake on.
type ClientPool struct{ *identity.FilePool }

// NewClientPool reads base and refreshed, which together must hold a certificate. Refreshed may be
// absent, or the empty path: a host the agent has not refreshed yet trusts base alone.
func NewClientPool(base, refreshed string) (*ClientPool, error) {
	p, err := identity.NewFilePool(base, refreshed)
	if err != nil {
		return nil, err
	}
	return &ClientPool{p}, nil
}

// TLSConfig is the host API's TLS configuration: it serves the pair serving holds and verifies a
// client's certificate against the pool, read again at each handshake when its files changed, and
// the revocation list when one is given.
func TLSConfig(serving *Serving, pool *ClientPool, revocations *identity.RevocationList) *tls.Config {
	base := ServerTLS(serving.GetCertificate, pool.Current(), revocations)
	conf := base.Clone()
	conf.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		c := base.Clone()
		c.ClientCAs = pool.Current()
		return c, nil
	}
	return conf
}
