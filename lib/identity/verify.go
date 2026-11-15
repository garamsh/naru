package identity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"
)

// PeerCheck is the one verification a naru TLS client makes of the certificate a server presents:
// the chain to Roots for Usage, the name rule, and the signer's revocation list.
type PeerCheck struct {
	Roots *x509.CertPool
	Usage x509.ExtKeyUsage
	// Names are the names the leaf may carry: it must name at least one. Empty checks no name.
	Names []string
	// Revocations is the list every certificate of the chain is checked against, with its max age.
	// Nil checks no revocation: the caller holds no revocation key.
	Revocations *RevocationList
	// Now is the clock the chain is checked at; it defaults to time.Now.
	Now func() time.Time
}

// Verify checks certs, the leaf first and then the intermediates the peer sent.
func (p PeerCheck) Verify(ctx context.Context, certs []*x509.Certificate) error {
	if len(certs) == 0 {
		return errors.New("the peer presented no certificate")
	}
	leaf := certs[0]
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: p.Roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{p.Usage}, CurrentTime: now})
	if err != nil {
		return fmt.Errorf("the peer's certificate: %w", err)
	}
	if len(p.Names) > 0 && !namesAny(leaf, p.Names) {
		return fmt.Errorf("the peer's certificate names none of %v", p.Names)
	}
	if p.Revocations == nil {
		return nil
	}
	return p.Revocations.Check(ctx, chains)
}

// VerifyConnection is Verify over the certificates of a handshake, for tls.Config.VerifyConnection
// of a client that sets InsecureSkipVerify so that this check replaces the built-in one.
func (p PeerCheck) VerifyConnection(cs tls.ConnectionState) error {
	return p.Verify(context.Background(), cs.PeerCertificates)
}

func namesAny(leaf *x509.Certificate, names []string) bool {
	for _, n := range names {
		if leaf.VerifyHostname(n) == nil {
			return true
		}
	}
	return false
}
