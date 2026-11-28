package signer

import (
	"crypto"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// authoriseHost verifies a request a host's certificate signed, against the host authority and the
// revocation list (ADR 0122): the host authority, not the operator's, issues a host's certificate.
func (s *Signer) authoriseHost(signed []byte, action string, body any) (identity.Identity, error) {
	a, err := s.ready()
	if err != nil {
		return identity.Identity{}, err
	}
	who, raw, err := identity.VerifyRequest(signed, action, pool(a.host.cert), &a.crl, s.now())
	if err != nil {
		return identity.Identity{}, err
	}
	if who.Role != identity.RoleHost || !namePattern.MatchString(who.Subject) {
		return identity.Identity{}, fmt.Errorf("%w: %s may not %s", ErrForbidden, who.Role, action)
	}
	return who, decode(raw, body)
}

// RenewHost issues a host a further certificate for the key of the current one that signed the
// request, in the last identity.HostRenewWindow of that certificate's life (ADR 0122). The new
// certificate has the current one's host ID and IP addresses, whatever the request asks, is valid a
// year, and is recorded among the host's serials, so the retire that follows its use revokes the
// others. Nothing is spent: a host whose answer was lost renews again, and its retire revokes every
// certificate it does not use.
func (s *Signer) RenewHost(signed []byte) (identity.Issued, error) {
	var req identity.HostRenewal
	who, err := s.authoriseHost(signed, identity.ActionRenewHost, &req)
	if err != nil {
		return identity.Issued{}, err
	}
	now := s.now()
	if due := who.NotAfter.Add(-identity.HostRenewWindow); now.Before(due) {
		return identity.Issued{}, fmt.Errorf("%w: renew from %s", identity.ErrHostNotDue, due.Format(time.RFC3339))
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return identity.Issued{}, err
	}
	if k, ok := csr.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !k.Equal(who.PublicKey) {
		return identity.Issued{}, fmt.Errorf("%w: a host renews under the key of its current certificate", ErrBadRequest)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	issued, err := issueLeaf(cur.host, now, who.Subject, identity.RoleHost, csr.PublicKey, serverAndClient,
		&x509.CertificateRequest{DNSNames: []string{who.Subject}, IPAddresses: who.IPAddresses})
	if err != nil {
		return identity.Issued{}, err
	}
	next := cur.withHost(who.Subject, issued.Serial)
	if err := commitFiles(s.dir, []stagedFile{{issuedFile, next.marshalIssued()}, {hostsFile, next.marshalHosts()}}); err != nil {
		return identity.Issued{}, err
	}
	s.auth = next
	s.log.Info("renewed a host's certificate", "host_id", who.Subject, "serial", issued.Serial, "previous_serial", who.Serial)
	return issued, nil
}

// RetireOwnHost revokes every serial the signer holds for a host but the one of the certificate
// that signed the request: the host's renewed certificate, which it signs with once it serves and
// reports with it (ADR 0122). It is RetireHost with the host's word for the serial it keeps.
func (s *Signer) RetireOwnHost(signed []byte) error {
	var body struct{}
	who, err := s.authoriseHost(signed, identity.ActionRetireOwnHost, &body)
	if err != nil {
		return err
	}
	return s.retire(who.Subject, who.Serial, who)
}
